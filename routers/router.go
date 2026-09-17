package routers

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	runtimeagent "dont/agent"
	"dont/controller"
	agentservice "dont/internal/agents"
	"dont/internal/authn"
	"dont/internal/automation"
	backupapi "dont/internal/backups"
	"dont/internal/capabilities"
	"dont/internal/chatlogs"
	"dont/internal/configuration"
	consoleapi "dont/internal/console"
	"dont/internal/containers"
	"dont/internal/distributedbackup"
	"dont/internal/dstruntime"
	dstinstall "dont/internal/dstserver"
	"dont/internal/entitycatalog"
	"dont/internal/fleetmember"
	"dont/internal/fleetoverview"
	"dont/internal/gameinstall"
	"dont/internal/gamenotifications"
	"dont/internal/gameupdate"
	"dont/internal/httpapi"
	"dont/internal/jobs"
	"dont/internal/kubernetesruntime"
	"dont/internal/logstream"
	"dont/internal/luajit"
	"dont/internal/modartifact"
	"dont/internal/modcontrol"
	"dont/internal/moddistribution"
	"dont/internal/modpublication"
	modservice "dont/internal/mods"
	"dont/internal/modupdates"
	"dont/internal/operationlease"
	"dont/internal/placementmigration"
	playerapi "dont/internal/players"
	"dont/internal/roomprovision"
	"dont/internal/rooms"
	"dont/internal/runtimeaudit"
	"dont/internal/runtimecpu"
	"dont/internal/runtimedriver"
	"dont/internal/runtimeevents"
	"dont/internal/runtimeguard"
	"dont/internal/runtimeobservation"
	"dont/internal/runtimeoverview"
	"dont/internal/saveimport"
	"dont/internal/shards"
	"dont/internal/structuredlogs"
	"dont/internal/systemsettings"
	"dont/internal/systemstatus"
	"dont/internal/topology"
	"dont/internal/worldmap"
	"dont/internal/worldstate"
	"dont/models"
	"dont/pkg/setting"
	legacyserver "dont/server"

	"github.com/gin-gonic/gin"
)

var testAdapterEnvironmentVariables = []string{
	"DST_ADMIN_TEST_AGENTS",
	"DST_ADMIN_TEST_SYSTEM_STATUS",
	"DST_ADMIN_TEST_SYSTEM_SETTINGS",
	"DST_ADMIN_TEST_CONTAINERS",
	"DST_ADMIN_TEST_CONTROL",
	"DST_ADMIN_TEST_MODS",
	"DST_ADMIN_TEST_PLAYERS",
	"DST_ADMIN_TEST_WORLD_STATE",
	"DST_ADMIN_TEST_UPDATE",
	"DST_ADMIN_TEST_MAP",
}

type runtimeInventoryCatalog interface {
	RuntimeTargetInventories(context.Context) ([]agentservice.RuntimeTargetInventory, error)
}

func syncRoomRuntimeCatalog(ctx context.Context, targets runtimeInventoryCatalog, catalog *rooms.Service) error {
	inventories, err := targets.RuntimeTargetInventories(ctx)
	if err != nil {
		return err
	}
	sources := make([]rooms.RuntimeCatalogSource, 0, len(inventories))
	for _, inventory := range inventories {
		observedAt := time.Time{}
		if inventory.ObservedAt != nil {
			observedAt = inventory.ObservedAt.UTC()
		}
		sources = append(sources, rooms.RuntimeCatalogSource{
			TargetID: inventory.Target.ID, Online: inventory.Target.Online, Available: inventory.Available,
			Stale: inventory.Stale, ObservedAt: observedAt, Inventory: inventory.Inventory,
		})
	}
	return catalog.SyncRuntimeCatalog(sources)
}

// InitRouter builds HTTP routes without starting process-scoped background tasks.
func InitRouter() (*gin.Engine, error) {
	application, err := initApplication(false)
	if err != nil {
		return nil, err
	}
	return application.Router(), nil
}

// InitApplication builds the production application and its managed lifecycle.
func InitApplication() (*Application, error) {
	return initApplication(true)
}

func initApplication(manageBackground bool) (*Application, error) {
	return initApplicationConfig(manageBackground, manageBackground, setting.CurrentSnapshot())
}

func initApplicationConfig(manageBackground, ownsDatabase bool, config setting.Snapshot) (*Application, error) {
	if err := setting.Validate(); err != nil {
		return nil, err
	}
	if err := validateTestAdapters(); err != nil {
		return nil, err
	}
	hooks := applicationHooks{}
	backgroundEnabled := manageBackground && os.Getenv("DST_ADMIN_ENV") != "test"
	_, databaseOpened, err := models.OpenConfigured()
	if err != nil {
		return nil, err
	}
	completed := false
	defer func() {
		if !completed {
			for i := len(hooks.stop) - 1; i >= 0; i-- {
				_ = hooks.stop[i](context.Background())
			}
		}
		if !completed && databaseOpened && ownsDatabase {
			_ = models.CloseDB()
		}
	}()
	tablePrefix := setting.Cfg.Section("database").Key("TABLE_PREFIX").String()
	authService, err := authn.NewServiceWithTablePrefix(models.DB(), tablePrefix)
	if err != nil {
		return nil, err
	}
	if err := authService.Migrate(); err != nil {
		return nil, err
	}
	authHandler := httpapi.NewAuthHandler(authService)
	roomStore := rooms.NewStore(models.DB(), tablePrefix)
	if err := roomStore.Migrate(); err != nil {
		return nil, err
	}
	savePath := config.Path("paths", "DST_SAVE_PATH", "DST_ADMIN_SAVE_PATH")
	backupPath := config.Path("paths", "DST_BACKUP_PATH", "DST_ADMIN_BACKUP_PATH")
	serverPath := config.Path("paths", "DST_SERVER_PATH", "DST_ADMIN_SERVER_PATH")
	ugcPath := config.Path("paths", "DST_UGC_PATH", "DST_ADMIN_UGC_PATH")
	steamCMDPath := config.Path("mod", "STEAM_CMD_PATH", "DST_ADMIN_STEAMCMD_PATH")
	luaFallbackPath := config.Path("mod", "LUA_SH_PATH", "DST_ADMIN_LUA_PATH")
	workshopContentPath := config.Path("mod", "WORKSHOP_CONTENT", "DST_ADMIN_WORKSHOP_CONTENT")
	workshopDownloadPath := config.Path("mod", "WORKSHOP_MOD_PATH", "DST_ADMIN_WORKSHOP_DOWNLOAD")
	steamAPIKey := config.String("mod", "STEAM_WEB_API_KEY", "DST_ADMIN_STEAM_API_KEY")
	steamAppID := config.String("mod", "APP_ID", "DST_ADMIN_STEAM_APP_ID")
	luaBinary := config.String("mod", "LUA_BINARY", "DST_ADMIN_LUA_BINARY")
	pythonBinary := config.String("mod", "PYTHON_BINARY", "DST_ADMIN_PYTHON_BINARY")
	if steamAppID == "" {
		steamAppID = "322330"
	}
	workshopDownloadPath, workshopContentPath, err = resolveWorkshopPaths(workshopDownloadPath, workshopContentPath, steamAppID)
	if err != nil {
		return nil, err
	}
	mapRendererPath := config.Path("map", "RENDERER_PATH", "DST_ADMIN_MAP_RENDERER_PATH")
	mapPath := config.Path("paths", "DST_MAP_PATH", "DST_ADMIN_MAP_PATH")
	if mapPath == "" {
		mapPath = backupPath + string(os.PathSeparator) + "maps"
	}
	serverMode := config.String("paths", "DST_SERVER_MODE", "DST_ADMIN_SERVER_MODE")
	localRuntimeDriver := strings.ToLower(strings.TrimSpace(config.String("runtime", "DRIVER", "DST_ADMIN_LOCAL_RUNTIME_DRIVER")))
	if localRuntimeDriver == "" {
		localRuntimeDriver = "native"
	}
	localInstallationID := strings.TrimSpace(config.String("runtime", "INSTALLATION_ID", "DST_ADMIN_LOCAL_INSTALLATION_ID"))
	if localInstallationID == "" {
		localInstallationID = "default"
	}
	localContainerEngine := strings.TrimSpace(config.String("runtime", "CONTAINER_ENGINE", "DST_ADMIN_LOCAL_CONTAINER_ENGINE"))
	if localContainerEngine == "" {
		localContainerEngine = "docker"
	}
	localContainerImage := strings.TrimSpace(config.String("runtime", "CONTAINER_IMAGE", "DST_ADMIN_LOCAL_CONTAINER_IMAGE"))
	if localContainerImage == "" {
		localContainerImage = "dst-admin/dst-runtime:dev"
	}
	localContainerHostSavePath := strings.TrimSpace(config.String("runtime", "CONTAINER_HOST_SAVE_PATH", "DST_ADMIN_LOCAL_CONTAINER_SAVE_SOURCE"))
	if localContainerHostSavePath == "" {
		localContainerHostSavePath = savePath
	}
	localContainerHostServerPath := strings.TrimSpace(config.String("runtime", "CONTAINER_HOST_SERVER_PATH", "DST_ADMIN_LOCAL_CONTAINER_SERVER_SOURCE"))
	if localContainerHostServerPath == "" {
		localContainerHostServerPath = serverPath
	}
	localContainerHostUGCPath := strings.TrimSpace(config.String("runtime", "CONTAINER_HOST_UGC_PATH", "DST_ADMIN_LOCAL_CONTAINER_UGC_SOURCE"))
	if localContainerHostUGCPath == "" {
		localContainerHostUGCPath = ugcPath
	}
	localConsoleSocket := strings.TrimSpace(config.String("runtime", "CONSOLE_SOCKET", "DST_ADMIN_LOCAL_CONSOLE_SOCKET"))
	if localConsoleSocket == "" {
		localConsoleSocket = "/run/dst-admin/tmux/tmux.sock"
	}
	localConsoleSession := strings.TrimSpace(config.String("runtime", "CONSOLE_SESSION", "DST_ADMIN_LOCAL_CONSOLE_SESSION"))
	if localConsoleSession == "" {
		localConsoleSession = "dst"
	}
	serverExecutablePath := serverPath
	serverInstallRoot := serverPath
	serverContentRoot := serverPath
	if layout, ok := dstinstall.Resolve(serverPath, serverMode); ok {
		serverExecutablePath = layout.Executable
		serverInstallRoot = layout.InstallRoot
		serverContentRoot = layout.ContentRoot
	}
	runtimeWorkshopContentPath := workshopContentPath
	deploymentProfile, err := deploymentProfileFromSnapshot(config, backupPath)
	if err != nil {
		return nil, fmt.Errorf("resolve deployment profile: %w", err)
	}
	roomCatalog, err := rooms.NewCatalog(savePath, roomStore)
	if err != nil {
		return nil, err
	}
	roomService := rooms.NewService(roomCatalog, roomStore)
	roomService.ConfigureLocalDiscovery(deploymentProfile.LocalExecutorEnabled)
	runtimeManager, err := dstruntime.NewManager(savePath, roomService)
	if err != nil {
		return nil, err
	}
	jobStore := jobs.NewStore(models.DB(), tablePrefix)
	if err := jobStore.Migrate(); err != nil {
		return nil, err
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		return nil, err
	}
	if _, err := jobService.Prune(jobs.DefaultRetentionPolicy); err != nil {
		return nil, fmt.Errorf("prune retained jobs: %w", err)
	}
	if backgroundEnabled {
		hooks.workers = append(hooks.workers, func(ctx context.Context) {
			jobService.RunRetention(ctx, 24*time.Hour, jobs.DefaultRetentionPolicy)
		})
	}
	agentStore := agentservice.NewStore(models.DB(), tablePrefix)
	if err := agentStore.Migrate(); err != nil {
		return nil, err
	}
	var agentGateway *legacyserver.Server
	if backgroundEnabled && deploymentProfile.ControllerEnabled {
		agentGateway, err = legacyserver.NewServer(&legacyserver.Config{
			KeyFile: config.ConfigPath, SecurityKey: config.String("server", "SECURITY_KEY", "DST_ADMIN_AGENT_SECURITY_KEY"),
		})
		if err != nil {
			return nil, fmt.Errorf("initialize embedded Agent gateway: %w", err)
		}
		defer func() {
			if completed {
				return
			}
			agentGateway.Stop()
			if controller.AgentServer == agentGateway {
				controller.AgentServer = nil
			}
		}()
		controller.AgentServer = agentGateway
		hooks.workers = append(hooks.workers, agentGateway.Maintain)
		hooks.stop = append(hooks.stop, func(context.Context) error {
			agentGateway.Stop()
			if controller.AgentServer == agentGateway {
				controller.AgentServer = nil
			}
			return nil
		})
	}
	var embeddedFleetMember *fleetmember.Member
	if backgroundEnabled && deploymentProfile.MemberEnabled {
		memberWorkshopContentPath := runtimeWorkshopContentPath
		if memberWorkshopContentPath == "" {
			memberWorkshopContentPath = workshopContentPath
		}
		embeddedFleetMember, err = fleetmember.New(fleetmember.Config{
			ControllerURL: deploymentProfile.ControllerURL, SecurityKey: deploymentProfile.MemberKey,
			NodeID: deploymentProfile.NodeID, StatePath: deploymentProfile.StatePath,
			Runtime: runtimeagent.RuntimeInstallation{
				ID: "default", Driver: "native", SavePath: savePath, ServerPath: serverPath,
				SteamCMDPath: steamCMDPath, UGCPath: ugcPath, WorkshopContentPath: memberWorkshopContentPath,
				ModCachePath: filepath.Join(deploymentProfile.StatePath, "mod-cache"),
				ModStatePath: filepath.Join(deploymentProfile.StatePath, "mod-state"), ServerMode: serverMode,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("initialize embedded Fleet member: %w", err)
		}
		hooks.start = append(hooks.start, embeddedFleetMember.Start)
		hooks.stop = append(hooks.stop, func(context.Context) error {
			embeddedFleetMember.Stop()
			return nil
		})
	}
	var agentTransport agentservice.Transport = agentservice.NewLegacyTransport(func() *legacyserver.Server { return agentGateway })
	if driver := os.Getenv("DST_ADMIN_TEST_AGENTS"); driver != "" {
		if os.Getenv("DST_ADMIN_ENV") != "test" || driver != "memory" {
			return nil, fmt.Errorf("DST_ADMIN_TEST_AGENTS is only available as memory in the test environment")
		}
		agentTransport = agentservice.NewMemoryTransport()
	}
	agentService, err := agentservice.NewService(agentStore, jobService, agentTransport)
	if err != nil {
		return nil, err
	}
	agentReleaseDir, err := agentservice.DefaultAgentReleaseDir()
	if err != nil {
		return nil, err
	}
	agentReleaseStore, err := agentservice.NewReleaseStore(agentReleaseDir)
	if err != nil {
		return nil, fmt.Errorf("initialize Agent release store: %w", err)
	}
	if err := agentService.ConfigureReleaseStore(agentReleaseStore); err != nil {
		return nil, err
	}
	if deploymentProfile.LocalExecutorEnabled {
		agentService.ConfigureLocalRuntime(agentservice.RuntimeConfig{
			InstallationID: localInstallationID, DisplayName: "本机", SavePath: savePath, BackupPath: backupPath, ServerPath: serverPath,
			UGCPath: ugcPath, SteamCMDPath: steamCMDPath, WorkshopContentPath: workshopContentPath,
			LuaBinary: luaBinary, LuaFallbackPath: luaFallbackPath, ServerMode: serverMode,
		})
	} else {
		agentService.DisableLocalRuntime()
	}
	runtimeObservationCoordinator, err := runtimeobservation.New(agentService)
	if err != nil {
		return nil, err
	}
	hooks.stop = append(hooks.stop, func(context.Context) error {
		runtimeObservationCoordinator.Close()
		return nil
	})
	reconcileRoomCatalog := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := syncRoomRuntimeCatalog(ctx, runtimeObservationCoordinator, roomService); err != nil {
			log.Printf("[RuntimeCatalog] synchronize room inventory: %v", err)
		}
	}
	agentService.AddRuntimeTopologyListener(reconcileRoomCatalog)
	if backgroundEnabled {
		reconcileRoomCatalog()
	}
	agentHandler := httpapi.NewAgentHandler(agentService)
	kubernetesRuntimeService := kubernetesruntime.LoadExperimentalService()
	kubernetesRuntimeHandler, err := httpapi.NewKubernetesRuntimeHandler(kubernetesRuntimeService)
	if err != nil {
		return nil, err
	}
	topologyStore := topology.NewStore(models.DB(), tablePrefix)
	if err := topologyStore.Migrate(); err != nil {
		return nil, err
	}
	topologyService, err := topology.NewService(roomService, runtimeObservationCoordinator, topologyStore, agentService)
	if err != nil {
		return nil, err
	}
	topologyHandler := httpapi.NewTopologyHandler(topologyService)
	operationLeaseService := operationlease.NewService(models.DB(), tablePrefix)
	if err := operationLeaseService.Migrate(); err != nil {
		return nil, err
	}
	if backgroundEnabled {
		hooks.workers = append(hooks.workers, func(ctx context.Context) {
			agentService.Watch(ctx, 5*time.Second, jobService.Notify)
		})
	}
	// Resource cards must describe the persistent DST volume. In All-in-One,
	// the container root filesystem is not the user's save/server data disk.
	var systemStatusProvider systemstatus.Provider = systemstatus.NewLocalProvider(savePath)
	if driver := os.Getenv("DST_ADMIN_TEST_SYSTEM_STATUS"); driver != "" {
		if os.Getenv("DST_ADMIN_ENV") != "test" || driver != "memory" {
			return nil, fmt.Errorf("DST_ADMIN_TEST_SYSTEM_STATUS is only available as memory in the test environment")
		}
		systemStatusProvider = systemstatus.NewMemoryProvider()
	}
	systemStatusService := systemstatus.NewService(systemStatusProvider)
	systemStatusService.SetDatabaseProvider(func() (systemstatus.DatabaseStatus, error) {
		status, statusErr := models.Status()
		return systemstatus.DatabaseStatus{
			Driver: status.Driver, JournalMode: status.JournalMode,
			BusyTimeoutMilliseconds: status.BusyTimeoutMilliseconds, ForeignKeys: status.ForeignKeys,
			MaxOpenConnections: status.MaxOpenConnections, MigrationVersion: status.MigrationVersion,
		}, statusErr
	})
	nodeResourceService, err := systemstatus.NewNodeResourceService(systemStatusService, agentService)
	if err != nil {
		return nil, err
	}
	systemStatusHandler := httpapi.NewSystemStatusHandler(systemStatusService, nodeResourceService)
	var systemSettingsRepository systemsettings.Repository = systemsettings.NewFileRepository(config.ConfigPath)
	if driver := os.Getenv("DST_ADMIN_TEST_SYSTEM_SETTINGS"); driver != "" {
		if os.Getenv("DST_ADMIN_ENV") != "test" || driver != "memory" {
			return nil, fmt.Errorf("DST_ADMIN_TEST_SYSTEM_SETTINGS is only available as memory in the test environment")
		}
		systemSettingsRepository = systemsettings.NewMemoryRepository()
	}
	systemSettingsService, err := systemsettings.NewService(systemSettingsRepository)
	if err != nil {
		return nil, err
	}
	systemSettingsHandler := httpapi.NewSystemSettingsHandler(systemSettingsService)
	beaconURL := strings.TrimSpace(config.String("catalog", "BEACON_URL", "DST_ADMIN_BEACON_URL"))
	if beaconURL == "" {
		beaconURL = "http://127.0.0.1:3000"
	}
	entityCatalogService, err := entitycatalog.New(beaconURL, nil)
	if err != nil {
		return nil, fmt.Errorf("initialize entity catalog: %w", err)
	}
	entityCatalogHandler := httpapi.NewEntityCatalogHandler(entityCatalogService)
	authService.SetPolicyProvider(func() authn.PasswordPolicy {
		preferences, runtimeErr := systemSettingsService.Runtime()
		if runtimeErr != nil {
			return authn.PasswordPolicy{MinimumLength: authn.MinPasswordLength, SessionTTL: 24 * time.Hour}
		}
		return authn.PasswordPolicy{
			MinimumLength:     preferences.MinPasswordLength,
			RequireComplexity: preferences.PasswordComplexity,
			SessionTTL:        preferences.SessionTimeout,
		}
	})
	authHandler.SetSecurityPolicyProvider(func() httpapi.LoginSecurityPolicy {
		preferences, runtimeErr := systemSettingsService.Runtime()
		if runtimeErr != nil {
			return httpapi.LoginSecurityPolicy{MaxAttempts: 5, BlockFor: 15 * time.Minute}
		}
		return httpapi.LoginSecurityPolicy{MaxAttempts: preferences.MaxLoginAttempts, BlockFor: 15 * time.Minute}
	})
	authHandler.SetUIPreferencesProvider(func() httpapi.UIPreferences {
		preferences, runtimeErr := systemSettingsService.Runtime()
		if runtimeErr != nil {
			return httpapi.UIPreferences{SystemName: "饥荒管理系统", Timezone: "Asia/Shanghai", DateFormat: "YYYY-MM-DD", ThemeColor: "#27272a"}
		}
		return httpapi.UIPreferences{
			SystemName: preferences.SystemName, Timezone: preferences.Timezone,
			DateFormat: preferences.DateFormat, ThemeColor: preferences.Theme,
		}
	})
	var containerTransport containers.Transport = containers.NewExecTransport()
	if driver := os.Getenv("DST_ADMIN_TEST_CONTAINERS"); driver != "" {
		if os.Getenv("DST_ADMIN_ENV") != "test" || driver != "memory" {
			return nil, fmt.Errorf("DST_ADMIN_TEST_CONTAINERS is only available as memory in the test environment")
		}
		containerTransport = containers.NewMemoryTransport()
	}
	containerService, err := containers.NewService(containerTransport, jobService)
	if err != nil {
		return nil, err
	}
	containerHandler := httpapi.NewContainerHandler(containerService)
	var shardControl interface {
		shards.Control
		consoleapi.Sender
		Status(context.Context, string, string) (shards.RuntimeStatus, error)
	}
	var localCPUBackend runtimedriver.NativeCPUBackend
	switch localRuntimeDriver {
	case "native":
		tmuxControl, controlErr := shards.NewTmuxControl(shards.TmuxConfig{
			SaveRoot: savePath, UGCDirectory: ugcPath, ServerPath: serverPath, ServerMode: serverMode,
			OwnerLabel: "本机 Runtime " + localInstallationID,
		})
		if controlErr != nil {
			return nil, controlErr
		}
		shardControl = tmuxControl
		hooks.stop = append(hooks.stop, func(context.Context) error { return tmuxControl.Close() })
	case "container":
		stateRoot := filepath.Join(backupPath, ".dst-admin-container-runtime")
		containerHost, controlErr := runtimeagent.NewContainerRuntimeHost(runtimeagent.ContainerRuntimeHostConfig{
			Installation: runtimeagent.RuntimeInstallation{
				ID: localInstallationID, Driver: "container", SavePath: savePath, ServerPath: serverPath,
				SteamCMDPath: steamCMDPath, UGCPath: ugcPath, WorkshopContentPath: runtimeWorkshopContentPath,
				ModCachePath: filepath.Join(stateRoot, "mod-cache"), ModStatePath: filepath.Join(stateRoot, "mod-state"),
				ServerMode: serverMode, ContainerEngine: localContainerEngine,
				ConsoleSocket: localConsoleSocket, ConsoleSession: localConsoleSession,
			},
			Image: localContainerImage, HostSavePath: localContainerHostSavePath,
			HostServerPath: localContainerHostServerPath, HostUGCPath: localContainerHostUGCPath,
			Timezone: os.Getenv("TZ"),
		})
		if controlErr != nil {
			return nil, fmt.Errorf("initialize local container Runtime: %w", controlErr)
		}
		agentService.ConfigureLocalContainerProcesses(containerHost)
		shardControl, localCPUBackend = containerHost, containerHost
	default:
		return nil, fmt.Errorf("unsupported local Runtime driver %q", localRuntimeDriver)
	}
	if driver := os.Getenv("DST_ADMIN_TEST_CONTROL"); driver != "" {
		if os.Getenv("DST_ADMIN_ENV") != "test" || driver != "memory" {
			return nil, fmt.Errorf("DST_ADMIN_TEST_CONTROL is only available as memory in the test environment")
		}
		shardControl = shards.NewMemoryControlWithLogRoot(savePath)
	}
	nativeRuntimeDriver, err := runtimedriver.NewNative(savePath, shardControl)
	if err != nil {
		return nil, err
	}
	trustedServerRoot, err := filepath.Abs(serverInstallRoot)
	if err != nil {
		return nil, err
	}
	if localCPUBackend == nil {
		localCPUBackend, err = runtimecpu.NewNative(runtimecpu.NativeConfig{ServerRoot: trustedServerRoot})
		if err != nil {
			return nil, err
		}
	}
	if err := nativeRuntimeDriver.ConfigureCPU(localCPUBackend); err != nil {
		return nil, err
	}
	agentRuntimeDriver, err := runtimedriver.NewAgent(agentService)
	if err != nil {
		return nil, err
	}
	runtimeDriverRouter, err := runtimedriver.NewRouter(topologyService, operationLeaseService, nativeRuntimeDriver, agentRuntimeDriver)
	if err != nil {
		return nil, err
	}
	if err := runtimeDriverRouter.ConfigureMutationObserver(runtimeObservationCoordinator); err != nil {
		return nil, err
	}
	localRuntimeEndpoint, err := runtimedriver.NewRuntimeEndpoint(nativeRuntimeDriver)
	if err != nil {
		return nil, err
	}
	if err := runtimeDriverRouter.RegisterEndpoint(runtimedriver.LocalTargetID, localInstallationID, localRuntimeEndpoint); err != nil {
		return nil, err
	}
	if err := topologyService.ConfigureCPUExecutor(runtimeDriverRouter); err != nil {
		return nil, err
	}
	localMutationGuard, err := runtimeguard.New(roomService, runtimeDriverRouter)
	if err != nil {
		return nil, err
	}
	placementMigrationService, err := placementmigration.New(topologyService, runtimeDriverRouter, operationLeaseService)
	if err != nil {
		return nil, err
	}
	if err := placementMigrationService.ConfigureMutationObserver(runtimeObservationCoordinator); err != nil {
		return nil, err
	}
	placementMigrationHandler, err := httpapi.NewPlacementMigrationHandler(placementMigrationService, roomService, jobService)
	if err != nil {
		return nil, err
	}
	roomProvisionStore := roomprovision.NewStore(models.DB(), tablePrefix)
	if err := roomProvisionStore.Migrate(); err != nil {
		return nil, err
	}
	roomProvisionService, err := roomprovision.NewCoordinator(roomService, topologyService, runtimeDriverRouter, operationLeaseService, roomProvisionStore)
	if err != nil {
		return nil, err
	}
	roomProvisionHandler, err := httpapi.NewRoomProvisionHandler(roomProvisionService, roomService, jobService)
	if err != nil {
		return nil, err
	}
	if backgroundEnabled {
		hooks.workers = append(hooks.workers, func(ctx context.Context) {
			if err := roomProvisionService.RecoverPending(ctx); err != nil {
				log.Printf("[RoomProvision] startup recovery failed: %v", err)
			}
		})
	}
	runtimeBridge, err := dstruntime.NewBridge(runtimeManager, shardControl, shardControl)
	if err != nil {
		return nil, err
	}
	distributedRuntimeBridge, err := dstruntime.NewDistributedBridge(runtimeBridge, runtimeDriverRouter)
	if err != nil {
		return nil, err
	}
	runtimeEventService, err := runtimeevents.New(distributedRuntimeBridge)
	if err != nil {
		return nil, err
	}
	runtimeOverviewService, err := runtimeoverview.New(topologyService, runtimeDriverRouter, distributedRuntimeBridge)
	if err != nil {
		return nil, err
	}
	fleetOverviewService, err := fleetoverview.New(topologyService, roomService, runtimeDriverRouter)
	if err != nil {
		return nil, err
	}
	runtimeObservabilityHandler, err := httpapi.NewRuntimeObservabilityHandler(runtimeEventService, runtimeOverviewService, fleetOverviewService, runtimeObservationCoordinator)
	if err != nil {
		return nil, err
	}
	shardOperations := shards.NewOperations(roomService, shardControl)
	if err := shardOperations.ConfigureRuntime(topologyService, runtimeDriverRouter, operationLeaseService); err != nil {
		return nil, err
	}
	gameNotificationStore := gamenotifications.NewStore(models.DB(), tablePrefix)
	if err := gameNotificationStore.Migrate(); err != nil {
		return nil, err
	}
	if err := gameNotificationStore.RecoverInterrupted(); err != nil {
		return nil, fmt.Errorf("recover interrupted game notifications: %w", err)
	}
	gameNotificationService, err := gamenotifications.NewService(roomService, runtimeDriverRouter, gameNotificationStore)
	if err != nil {
		return nil, err
	}
	shardOperations.ConfigureNotifier(gameNotificationService)
	gameNotificationHandler, err := httpapi.NewGameNotificationHandler(gameNotificationService, jobService)
	if err != nil {
		return nil, err
	}
	runtimeAuditStore := runtimeaudit.NewStore(models.DB(), tablePrefix)
	if err := runtimeAuditStore.Migrate(); err != nil {
		return nil, err
	}
	runtimeAuditService, err := runtimeaudit.NewService(roomService, shardOperations, runtimeAuditStore)
	if err != nil {
		return nil, err
	}
	shardOperations.ConfigureObserver(runtimeAuditService)
	if backgroundEnabled {
		hooks.workers = append(hooks.workers, func(ctx context.Context) {
			runtimeAuditService.Watch(ctx, 5*time.Second)
		})
	}
	roomHandler := httpapi.NewRoomHandler(roomService, shardOperations, jobService, runtimeAuditService)
	if err := roomHandler.ConfigureRoomRecovery(runtimeDriverRouter); err != nil {
		return nil, err
	}
	runtimeAuditHandler := httpapi.NewRuntimeAuditHandler(runtimeAuditService)
	runtimeHandler := httpapi.NewDSTRuntimeHandler(runtimeManager, roomService, shardOperations, distributedRuntimeBridge)
	jobHandler := httpapi.NewJobHandler(jobService)
	logService, err := logstream.NewService(savePath, roomService, runtimeDriverRouter)
	if err != nil {
		return nil, err
	}
	logHandler := httpapi.NewLogHandler(logService)
	chatLogStore := chatlogs.NewStore(models.DB(), tablePrefix)
	if err := chatLogStore.Migrate(); err != nil {
		return nil, err
	}
	chatLogService, err := chatlogs.NewService(roomService, logService, chatlogs.WithPersistence(chatLogStore, runtimeDriverRouter))
	if err != nil {
		return nil, err
	}
	chatLogHandler := httpapi.NewChatLogHandler(chatLogService, jobService)
	structuredLogStore := structuredlogs.NewStore(models.DB(), tablePrefix)
	if err := structuredLogStore.Migrate(); err != nil {
		return nil, err
	}
	structuredLogService, err := structuredlogs.NewService(roomService, logService, structuredLogStore)
	if err != nil {
		return nil, err
	}
	structuredLogHandler := httpapi.NewStructuredLogHandler(structuredLogService, jobService)
	commandStore := consoleapi.NewStore(models.DB(), tablePrefix)
	if err := commandStore.Migrate(); err != nil {
		return nil, err
	}
	commandService, err := consoleapi.NewService(roomService, runtimeDriverRouter, commandStore, distributedRuntimeBridge)
	if err != nil {
		return nil, err
	}
	consoleHandler := httpapi.NewConsoleHandler(commandService)
	backupStore := backupapi.NewStore(models.DB(), tablePrefix)
	if err := backupStore.Migrate(); err != nil {
		return nil, err
	}
	backupService, err := backupapi.NewService(savePath, backupPath, roomService, shardControl, backupStore)
	if err != nil {
		return nil, err
	}
	backupService.ConfigureMutationGuard(localMutationGuard)
	backupHandler := httpapi.NewBackupHandler(backupService, jobService)
	distributedBackupStore := distributedbackup.NewStore(models.DB(), tablePrefix)
	if err := distributedBackupStore.Migrate(); err != nil {
		return nil, err
	}
	distributedBackupService, err := distributedbackup.NewCoordinator(backupPath, roomService, topologyService, runtimeDriverRouter, operationLeaseService, distributedBackupStore)
	if err != nil {
		return nil, err
	}
	if err := distributedBackupService.ConfigureMutationObserver(runtimeObservationCoordinator); err != nil {
		return nil, err
	}
	distributedBackupHandler, err := httpapi.NewDistributedBackupHandler(distributedBackupService, jobService)
	if err != nil {
		return nil, err
	}
	if backgroundEnabled {
		hooks.workers = append(hooks.workers, func(ctx context.Context) {
			if err := distributedBackupService.Recover(ctx); err != nil {
				log.Printf("[DistributedBackup] startup recovery failed: %v", err)
			}
		})
	}
	saveImportStore := saveimport.NewStore(models.DB(), tablePrefix)
	if err := saveImportStore.Migrate(); err != nil {
		return nil, err
	}
	configurationBackups, err := configuration.NewHybridBackupCreator(backupService, distributedBackupService)
	if err != nil {
		return nil, err
	}
	configurationService, err := configuration.NewService(savePath, roomService, configurationBackups)
	if err != nil {
		return nil, err
	}
	configurationStateStore := configuration.NewStateStore(models.DB(), tablePrefix)
	if err := configurationStateStore.Migrate(); err != nil {
		return nil, err
	}
	if err := configurationService.ConfigureStateRepository(configurationStateStore); err != nil {
		return nil, err
	}
	configurationPublisher, err := configuration.NewRemotePublisher(topologyService, runtimeDriverRouter, operationLeaseService)
	if err != nil {
		return nil, err
	}
	if err := configurationPublisher.ConfigureMutationObserver(runtimeObservationCoordinator); err != nil {
		return nil, err
	}
	shardLinkReconciler, err := configuration.NewShardLinkReconciler(topologyService, configurationPublisher, runtimeObservationCoordinator, configurationService)
	if err != nil {
		return nil, err
	}
	if err := topologyHandler.ConfigureShardLinkApplier(shardLinkReconciler); err != nil {
		return nil, err
	}
	if err := configurationService.ConfigurePublisher(configurationPublisher); err != nil {
		return nil, err
	}
	if err := configurationService.ConfigureReader(runtimeDriverRouter); err != nil {
		return nil, err
	}
	configurationHandler := httpapi.NewConfigurationHandler(configurationService, jobService)
	metadataStore := modservice.NewMetadataStore(models.DB(), tablePrefix)
	if err := metadataStore.Migrate(); err != nil {
		return nil, fmt.Errorf("initialize Workshop metadata: %w", err)
	}
	steamMetadata := modservice.NewSteamProvider(steamAPIKey, steamAppID)
	steamMetadata.ConfigureMetadataStore(metadataStore)
	var modMetadata modservice.MetadataProvider = steamMetadata
	var modRunner modservice.DownloadRunner = modservice.NewSteamCMDRunner(steamCMDPath, workshopDownloadPath, steamAppID)
	var modParser modservice.ModInfoParser = modservice.NewDualParserWithPython(luaBinary, pythonBinary, luaFallbackPath)
	if driver := os.Getenv("DST_ADMIN_TEST_MODS"); driver != "" {
		if os.Getenv("DST_ADMIN_ENV") != "test" || driver != "memory" {
			return nil, fmt.Errorf("DST_ADMIN_TEST_MODS is only available as memory in the test environment")
		}
		modMetadata = modservice.NewMemoryMetadataProvider()
		modRunner = &modservice.MemoryDownloadRunner{InstallRoot: workshopDownloadPath, AppID: steamAppID}
		modParser = &modservice.MemoryModInfoParser{Base: modParser}
	}
	modService, err := modservice.NewService(modservice.Config{
		SaveRoot: savePath, ServerRoot: serverContentRoot, WorkshopContentRoot: workshopContentPath,
		UGCRoot: ugcPath, AppID: steamAppID,
	}, roomService, shardControl, backupService, modMetadata, modParser, modRunner)
	if err != nil {
		return nil, err
	}
	hooks.stop = append(hooks.stop, func(context.Context) error {
		return modService.Close()
	})
	modService.ConfigureMutationGuard(localMutationGuard)
	modHandler := httpapi.NewModHandler(modService, jobService)
	modPublicationRoot := filepath.Join(backupPath, ".mod-publication")
	modReplicaStore := modpublication.NewReplicaStore(models.DB(), tablePrefix)
	if err := modReplicaStore.Migrate(); err != nil {
		return nil, err
	}
	localModManager, err := openLocalModManager(moddistribution.Config{
		CacheRoot: filepath.Join(modPublicationRoot, "cache"), StateRoot: filepath.Join(modPublicationRoot, "state"),
		NodeID: "local", ReserveBytes: 16 << 20,
		Installations: []moddistribution.TrustedInstallation{{
			ID: "default", NodeID: "local", ServerPath: serverContentRoot, SavePath: savePath,
			WorkshopContentPath: runtimeWorkshopContentPath,
		}},
	})
	if err != nil {
		return nil, err
	}
	if backgroundEnabled {
		hooks.workers = append(hooks.workers, func(ctx context.Context) {
			recoverLocalModManager(ctx, localModManager)
		})
	}
	modArtifactService, err := modartifact.NewService(localModManager, filepath.Join(modPublicationRoot, "bundles"))
	if err != nil {
		return nil, fmt.Errorf("initialize Mod artifact service: %w", err)
	}
	modArtifactHandler := httpapi.NewModArtifactHandler(modArtifactService)
	modSnapshotSource, err := modcontrol.NewSnapshotSource(roomService, topologyService, modService, agentRuntimeDriver, savePath)
	if err != nil {
		return nil, fmt.Errorf("initialize Mod publication snapshots: %w", err)
	}
	modContentSource, err := modcontrol.NewContentSource(localModManager, workshopContentPath)
	if err != nil {
		return nil, fmt.Errorf("initialize Mod publication content: %w", err)
	}
	modPublicationRuntime, err := modcontrol.NewRuntime(
		modSnapshotSource, localModManager, agentRuntimeDriver,
		filepath.Join(modPublicationRoot, "cache"), filepath.Join(modPublicationRoot, "transfers"),
	)
	if err != nil {
		return nil, fmt.Errorf("initialize Mod publication runtime: %w", err)
	}
	if err := modPublicationRuntime.ConfigureMutationObserver(runtimeObservationCoordinator); err != nil {
		return nil, err
	}
	if err := modPublicationRuntime.ConfigureReplicaStore(modReplicaStore); err != nil {
		return nil, err
	}
	if err := modPublicationRuntime.ConfigureArtifactSource(modArtifactService); err != nil {
		return nil, err
	}
	modPublicationPlanner, err := modpublication.NewPlanner(
		modSnapshotSource, modSnapshotSource, modContentSource, modPublicationRuntime, "1.0.0",
	)
	if err != nil {
		return nil, fmt.Errorf("initialize Mod publication planner: %w", err)
	}
	modPublicationStore := modpublication.NewStore(models.DB(), tablePrefix)
	if err := modPublicationStore.Migrate(); err != nil {
		return nil, err
	}
	modLeaseAdapter, err := modcontrol.NewLeaseAdapter(operationLeaseService)
	if err != nil {
		return nil, err
	}
	modInstallationUpdater, err := modcontrol.NewInstallationUpdater(modService, modContentSource, modPublicationRuntime, modLeaseAdapter)
	if err != nil {
		return nil, fmt.Errorf("initialize machine Mod updater: %w", err)
	}
	modHandler.ConfigureInstallationUpdater(modInstallationUpdater)
	modBackupAdapter, err := modcontrol.NewBackupAdapter(distributedBackupService)
	if err != nil {
		return nil, err
	}
	modActivationRuntime, err := modcontrol.NewActivationRuntime(runtimeDriverRouter)
	if err != nil {
		return nil, fmt.Errorf("initialize Mod activation runtime: %w", err)
	}
	modPublicationCoordinator, err := modpublication.NewCoordinator(
		modPublicationPlanner, modPublicationRuntime, modLeaseAdapter, modBackupAdapter,
		modPublicationStore, 5*time.Minute, modActivationRuntime,
	)
	if err != nil {
		return nil, fmt.Errorf("initialize Mod publication coordinator: %w", err)
	}
	if err := modPublicationCoordinator.ConfigureReplicaStore(modReplicaStore); err != nil {
		return nil, fmt.Errorf("initialize Mod replica projection: %w", err)
	}
	modPublicationCoordinator.ConfigureNotifier(gameNotificationService)
	modControlService, err := modcontrol.NewService(modSnapshotSource, modService, modPublicationCoordinator)
	if err != nil {
		return nil, fmt.Errorf("initialize Mod publication service: %w", err)
	}
	modConfigurationModes := modcontrol.NewConfigurationModeStore(models.DB(), tablePrefix)
	if err := modConfigurationModes.Migrate(); err != nil {
		return nil, err
	}
	modControlService.ConfigureConfigurationModes(modConfigurationModes)
	if err := modControlService.ConfigureConvergence(modPublicationRuntime); err != nil {
		return nil, err
	}
	if err := modControlService.ConfigureRuntimeFileObserver(modPublicationRuntime); err != nil {
		return nil, err
	}
	if err := modControlService.ConfigureReplicaReader(modReplicaStore); err != nil {
		return nil, err
	}
	if err := modControlService.ConfigureInstallationContentFetcher(modInstallationUpdater); err != nil {
		return nil, err
	}
	if err := modControlService.ConfigureConfigurationPublisher(configurationPublisher); err != nil {
		return nil, err
	}
	modHandler.ConfigureRoomInstaller(modControlService)
	if err := placementMigrationService.ConfigureModReconciler(modControlService); err != nil {
		return nil, err
	}
	modPublicationHandler, err := httpapi.NewModPublicationHandler(modControlService, jobService)
	if err != nil {
		return nil, err
	}
	modPublicationHandler.ConfigureReplicaReader(modControlService)
	modHandler.ConfigurePlacementReader(modControlService)
	if backgroundEnabled {
		hooks.workers = append(hooks.workers, func(ctx context.Context) {
			if _, err := modControlService.Recover(ctx); err != nil {
				log.Printf("[ModPublication] startup recovery failed: %v", err)
			}
		})
	}
	playerStore := playerapi.NewStore(models.DB(), tablePrefix)
	if err := playerStore.Migrate(); err != nil {
		return nil, err
	}
	var playerProbe playerapi.Probe
	if os.Getenv("DST_ADMIN_TEST_PLAYERS") != "" {
		if os.Getenv("DST_ADMIN_ENV") != "test" || os.Getenv("DST_ADMIN_TEST_PLAYERS") != "memory" {
			return nil, fmt.Errorf("DST_ADMIN_TEST_PLAYERS is only available as memory in the test environment")
		}
		playerProbe = playerapi.MemoryProbe{}
	} else {
		nativeProbe, probeErr := playerapi.NewLogProbe(savePath, roomService)
		if probeErr != nil {
			return nil, probeErr
		}
		fallbackProbe, probeErr := playerapi.NewConsoleProbe(savePath, roomService, shardControl)
		if probeErr != nil {
			return nil, probeErr
		}
		telemetryProbe, probeErr := playerapi.NewTelemetryProbe(nativeProbe, distributedRuntimeBridge, fallbackProbe, runtimeDriverRouter)
		if probeErr != nil {
			return nil, probeErr
		}
		telemetryProbe.ConfigureRemoteFallback(playerapi.NewRuntimeConsoleProbe(runtimeDriverRouter))
		telemetryProbe.ConfigureHistory(playerapi.NewRuntimeHistoryProbe(runtimeDriverRouter))
		playerProbe = telemetryProbe
	}
	playerService, err := playerapi.NewService(roomService, shardOperations, runtimeDriverRouter, configurationService, playerStore, playerProbe, distributedRuntimeBridge)
	if err != nil {
		return nil, err
	}
	gameNotificationService.ConfigureOnlineCounter(playerapi.NewRuntimeOnlineCounter(
		roomService, runtimeDriverRouter, playerapi.NewRuntimeRosterProbe(runtimeDriverRouter),
	))
	if backgroundEnabled {
		hooks.workers = append(hooks.workers, func(ctx context.Context) {
			playerService.RunBanExpiryScheduler(ctx, time.Minute)
		})
	}
	playerHandler := httpapi.NewPlayerHandler(playerService, jobService)
	modUpdateStore := modupdates.NewStore(models.DB(), tablePrefix)
	if err := modUpdateStore.Migrate(); err != nil {
		return nil, err
	}
	modUpdateService, err := modupdates.NewService(modUpdateStore, roomService, modControlService, modControlService, modupdates.NewWorldRestarter(roomService, shardOperations), playerService, jobService)
	if err != nil {
		return nil, fmt.Errorf("initialize Mod update service: %w", err)
	}
	modHandler.ConfigureInstallationUpdateRefresher(modUpdateService)
	modUpdateService.ConfigureAnnouncer(modupdates.AnnounceFunc(func(ctx context.Context, roomID, message, jobID string) error {
		_, failures, _, notifyErr := gameNotificationService.SendAutomation(ctx, roomID, message, jobID)
		if notifyErr != nil {
			return notifyErr
		}
		if failures > 0 {
			return fmt.Errorf("%d 个世界未收到模组更新公告", failures)
		}
		return nil
	}))
	modUpdateHandler, err := httpapi.NewModUpdateHandler(modUpdateService)
	if err != nil {
		return nil, err
	}
	if backgroundEnabled {
		hooks.workers = append(hooks.workers, modUpdateService.Run)
	}
	worldStateStore := worldstate.NewStore(models.DB(), tablePrefix)
	if err := worldStateStore.Migrate(); err != nil {
		return nil, err
	}
	var worldStateSampler worldstate.Sampler
	if driver := os.Getenv("DST_ADMIN_TEST_WORLD_STATE"); driver != "" {
		if os.Getenv("DST_ADMIN_ENV") != "test" || driver != "memory" {
			return nil, fmt.Errorf("DST_ADMIN_TEST_WORLD_STATE is only available as memory in the test environment")
		}
		worldStateSampler = worldstate.MemorySampler{}
	} else {
		worldStateSampler, err = worldstate.NewRuntimeSampler(distributedRuntimeBridge)
		if err != nil {
			return nil, err
		}
	}
	worldStateService, err := worldstate.NewService(roomService, shardOperations, worldStateStore, worldStateSampler)
	if err != nil {
		return nil, err
	}
	worldStateHandler := httpapi.NewWorldStateHandler(worldStateService)
	automationStore := automation.NewStore(models.DB(), tablePrefix)
	if err := automationStore.Migrate(); err != nil {
		return nil, err
	}
	automationExecutor, err := automation.NewDomainExecutor(shardOperations, backupService, commandService, playerService, structuredLogService, worldStateService, runtimeAuditService)
	if err != nil {
		return nil, err
	}
	if err := automationExecutor.ConfigureNotifications(gameNotificationService); err != nil {
		return nil, err
	}
	automationService, err := automation.NewService(roomService, automationStore, jobService, automationExecutor)
	if err != nil {
		return nil, err
	}
	legacyMigration, err := automation.MigrateLegacyCronTasks(models.DB(), tablePrefix, roomService, automationService)
	if err != nil {
		return nil, err
	}
	if legacyMigration.Examined > 0 {
		log.Printf("[AutomationMigration] checked=%d migrated=%d existing=%d skipped=%d", legacyMigration.Examined, legacyMigration.Migrated, legacyMigration.Existing, len(legacyMigration.Skipped))
		for _, skipped := range legacyMigration.Skipped {
			log.Printf("[AutomationMigration] skipped legacy task id=%d name=%q: %s", skipped.ID, skipped.Name, skipped.Reason)
		}
	}
	managedRooms, err := roomService.List()
	if err != nil {
		return nil, err
	}
	for _, room := range managedRooms {
		if !room.Managed {
			continue
		}
		if _, changed, defaultErr := automationService.EnsureDefaultPlayerRefresh(room.ID); defaultErr != nil {
			return nil, defaultErr
		} else if changed {
			log.Printf("[AutomationDefaults] ensured one-minute player refresh for room=%s", room.ID)
		}
	}
	automationScheduler := automation.NewScheduler(automationStore, automationService)
	if backgroundEnabled {
		hooks.start = append(hooks.start, automationScheduler.Start)
		hooks.stop = append(hooks.stop, automationScheduler.Close)
	}
	roomService.SetManagedRoomLifecycle(func(roomID string) {
		if _, changed, ensureErr := automationService.EnsureDefaultPlayerRefresh(roomID); ensureErr != nil {
			log.Printf("[RoomLifecycle] ensure player refresh room=%s: %v", roomID, ensureErr)
			return
		} else if changed && backgroundEnabled {
			if reloadErr := automationScheduler.Reload(); reloadErr != nil {
				log.Printf("[RoomLifecycle] reload automation after managing room=%s: %v", roomID, reloadErr)
			}
		}
	}, func(roomID string) {
		if cleanupErr := automationService.DeleteRoomTasks(roomID); cleanupErr != nil {
			log.Printf("[RoomLifecycle] finalize automation room=%s: %v", roomID, cleanupErr)
			return
		}
		if backgroundEnabled {
			if reloadErr := automationScheduler.Reload(); reloadErr != nil {
				log.Printf("[RoomLifecycle] reload automation after unmanaging room=%s: %v", roomID, reloadErr)
			}
		}
	})
	saveImportService, err := saveimport.NewServiceWithCoordinator(saveimport.Config{
		SaveRoot: savePath, ImportRoot: filepath.Join(backupPath, ".imports"), WorkshopRoot: workshopContentPath,
	}, saveImportStore, roomService, shardControl, backupService, modService, distributedBackupService, localMutationGuard)
	if err != nil {
		return nil, err
	}
	if err := saveImportService.ConfigurePortAllocator(topologyService); err != nil {
		return nil, err
	}
	saveImportHandler := httpapi.NewSaveImportHandler(saveImportService, jobService)
	automationHandler := httpapi.NewAutomationHandler(automationService, automationScheduler)
	if backgroundEnabled {
		backupScheduler := backupapi.NewScheduler(backupService, jobService)
		hooks.workers = append(hooks.workers, backupScheduler.Run)
	}
	gameUpdateStore := gameupdate.NewStore(models.DB(), tablePrefix)
	if err := gameUpdateStore.Migrate(); err != nil {
		return nil, err
	}
	var updateRunner gameupdate.CommandRunner = gameupdate.ExecRunner{}
	var latestChecker gameupdate.LatestChecker = gameupdate.NewSteamVersionChecker(steamCMDPath)
	var officialReleaseChecker gameupdate.OfficialReleaseChecker = gameupdate.NewKleiReleaseChecker(gameUpdateStore)
	if driver := os.Getenv("DST_ADMIN_TEST_UPDATE"); driver != "" {
		if os.Getenv("DST_ADMIN_ENV") != "test" || driver != "memory" {
			return nil, fmt.Errorf("DST_ADMIN_TEST_UPDATE is only available as memory in the test environment")
		}
		version := os.Getenv("DST_ADMIN_TEST_LATEST_VERSION")
		if version == "" {
			version = "200"
		}
		updateRunner = gameupdate.NewMemoryRunner(serverInstallRoot, version)
		latestChecker = gameupdate.NewMemoryLatestChecker(version)
		officialReleaseChecker = nil
	}
	gameUpdateService, err := gameupdate.NewService(
		gameupdate.Config{
			ServerPath: serverExecutablePath, SteamCMDPath: steamCMDPath,
			DisableUpdate:          strings.EqualFold(strings.TrimSpace(os.Getenv("DST_ADMIN_DISABLE_LOCAL_GAME_UPDATE")), "true"),
			OfficialReleaseChecker: officialReleaseChecker,
		},
		roomService, shardControl, backupService, gameUpdateStore, updateRunner, latestChecker, runtimeAuditService,
	)
	if err != nil {
		return nil, err
	}
	gameUpdateService.ConfigureNotifier(gameNotificationService)
	gameUpdateHandler := httpapi.NewGameUpdateHandler(gameUpdateService, jobService)
	luajitReleaseDir, err := luajit.DefaultReleaseDir()
	if err != nil {
		return nil, err
	}
	luajitStore, err := luajit.NewStore(luajitReleaseDir)
	if err != nil {
		return nil, err
	}
	luajitTransfers, err := luajit.NewStore(filepath.Join(luajitReleaseDir, "controller-transfers"))
	if err != nil {
		return nil, err
	}
	luajitService := luajit.NewService(luajitStore, luajitTransfers, agentService, jobService, operationLeaseService)
	luajitHandler := httpapi.NewLuaJITHandler(luajitService, luajitTransfers)
	gameInstallationHandler := httpapi.NewGameInstallationHandler(gameinstall.NewService(agentService, jobService, operationLeaseService))
	gameReleaseStore := gameupdate.NewReleaseStore(models.DB(), tablePrefix)
	if err := gameReleaseStore.Migrate(); err != nil {
		return nil, err
	}
	gameReleaseSnapshot, err := gameupdate.NewTopologyReleaseSnapshot(roomService, topologyService, runtimeObservationCoordinator)
	if err != nil {
		return nil, fmt.Errorf("initialize game release topology snapshot: %w", err)
	}
	localGameVersionDriver, err := gameupdate.NewLocalGameVersionDriver(gameUpdateService)
	if err != nil {
		return nil, fmt.Errorf("initialize local game version Runtime: %w", err)
	}
	localRuntimeEndpoint.GameVersion = localGameVersionDriver
	gameUpdateHandler.ConfigureInstalledVersions(gameupdate.NewInstalledVersionService(roomService, topologyService, agentService, localGameVersionDriver, agentRuntimeDriver))
	if err := runtimeDriverRouter.RegisterEndpoint(runtimedriver.LocalTargetID, localInstallationID, localRuntimeEndpoint); err != nil {
		return nil, fmt.Errorf("register local game version Runtime: %w", err)
	}
	gameReleaseRuntime, err := gameupdate.NewDriverReleaseRuntime(runtimeDriverRouter)
	if err != nil {
		return nil, fmt.Errorf("initialize game release runtime: %w", err)
	}
	gameReleaseProtection, err := gameupdate.NewDistributedReleaseProtection(distributedBackupService)
	if err != nil {
		return nil, fmt.Errorf("initialize game release protection: %w", err)
	}
	gameReleasePlanner, err := gameupdate.NewReleasePlanner(gameReleaseSnapshot, gameReleaseRuntime, latestChecker, gameupdate.DefaultUpdateHeadroom, officialReleaseChecker)
	if err != nil {
		return nil, fmt.Errorf("initialize game release planner: %w", err)
	}
	gameReleaseCoordinator, err := gameupdate.NewReleaseCoordinator(
		gameReleasePlanner, gameReleaseRuntime, operationLeaseService, gameReleaseProtection, gameReleaseStore,
	)
	if err != nil {
		return nil, fmt.Errorf("initialize game release coordinator: %w", err)
	}
	if err := gameReleaseCoordinator.ConfigureMutationObserver(runtimeObservationCoordinator); err != nil {
		return nil, err
	}
	gameReleaseCoordinator.ConfigureNotifier(gameNotificationService)
	if err := gameUpdateHandler.ConfigureReleases(gameReleaseCoordinator); err != nil {
		return nil, err
	}
	worldMapStore := worldmap.NewStore(models.DB(), tablePrefix)
	if err := worldMapStore.Migrate(); err != nil {
		return nil, err
	}
	var mapRenderer worldmap.Renderer = worldmap.NewExecRenderer(mapRendererPath, serverPath)
	if driver := os.Getenv("DST_ADMIN_TEST_MAP"); driver != "" {
		if os.Getenv("DST_ADMIN_ENV") != "test" || driver != "memory" {
			return nil, fmt.Errorf("DST_ADMIN_TEST_MAP is only available as memory in the test environment")
		}
		mapRenderer = worldmap.NewMemoryRenderer()
	}
	worldMapService, err := worldmap.NewService(worldmap.Config{SaveRoot: savePath, MapRoot: mapPath, Retention: 3}, roomService, worldMapStore, mapRenderer)
	if err != nil {
		return nil, err
	}
	if err := worldMapService.ConfigureRemote(runtimeDriverRouter, operationLeaseService); err != nil {
		return nil, err
	}
	worldMapHandler := httpapi.NewWorldMapHandler(worldMapService, jobService)
	if err := models.RecordMigration(models.CurrentMigrationVersion); err != nil {
		return nil, err
	}
	capabilityConfig := capabilities.Config{
		SavePath: savePath, BackupPath: backupPath, ServerPath: serverPath, ServerMode: serverMode, SteamCMDPath: steamCMDPath,
		LuaBinary: luaBinary, PythonBinary: pythonBinary, LuaFallbackPath: luaFallbackPath,
		MapRendererPath: mapRendererPath, MapPath: mapPath,
		DeploymentProfile: deploymentProfile,
	}
	if embeddedFleetMember != nil {
		capabilityConfig.FleetMemberConnected = embeddedFleetMember.Connected
	}
	idempotencyStore := httpapi.NewIdempotencyStore(15*time.Minute, 2048)
	router := gin.New()
	if err := router.SetTrustedProxies(nil); err != nil {
		return nil, err
	}

	router.Use(
		httpapi.RequestContext(),
		httpapi.Recovery(),
		httpapi.SameOrigin(),
	)
	router.NoRoute(httpapi.NotFound)
	if agentGateway != nil {
		router.GET("/agent", gin.WrapH(agentGateway.Handler()))
	}
	luajitHandler.RegisterDownloads(router)
	agentHandler.RegisterDownloads(router)
	modArtifactHandler.RegisterDownloads(router)

	api := router.Group("/api", httpapi.AdminIPPolicy(func() string {
		preferences, runtimeErr := systemSettingsService.Runtime()
		if runtimeErr != nil {
			return ""
		}
		return preferences.IPWhitelist
	}), httpapi.RequireSession(authService))
	{
		// Reject local Member mutations before validating write-specific headers so
		// every blocked request reports the same read-only contract.
		v2 := api.Group("/v2", httpapi.FleetMemberPolicy(deploymentProfile.MemberEnabled), idempotencyStore.Middleware())
		v2.Use(httpapi.RuntimeTargetBoundary())
		authHandler.Register(v2.Group("/auth"))
		gameNotificationHandler.Register(v2)
		v2.GET("/system/capabilities", httpapi.CapabilitiesProvider(func() capabilities.Report {
			return capabilities.Probe(capabilityConfig)
		}))
		v2.GET("/system/setup-checks", httpapi.SetupChecks(func() capabilities.Readiness {
			readiness := capabilities.ProbeReadiness(capabilityConfig, func() (int, error) {
				items, err := roomService.List()
				return len(items), err
			})
			completed, auditErr := runtimeAuditService.FirstSuccessfulStart()
			if auditErr != nil {
				log.Printf("[SetupChecks] read first successful start: %v", auditErr)
			} else if completed != nil {
				completedAt := completed.OccurredAt
				readiness.Onboarding = capabilities.Onboarding{FirstStartCompleted: true, CompletedAt: &completedAt}
			}
			return readiness
		}))
		roomHandler.Register(v2)
		runtimeAuditHandler.Register(v2)
		runtimeHandler.Register(v2)
		runtimeObservabilityHandler.Register(v2)
		jobHandler.Register(v2)
		logHandler.Register(v2)
		chatLogHandler.Register(v2)
		structuredLogHandler.Register(v2)
		consoleHandler.Register(v2)
		playerHandler.Register(v2)
		worldStateHandler.Register(v2)
		automationHandler.Register(v2)
		agentHandler.Register(v2)
		kubernetesRuntimeHandler.Register(v2)
		topologyHandler.Register(v2)
		placementMigrationHandler.Register(v2)
		roomProvisionHandler.Register(v2)
		systemStatusHandler.Register(v2)
		systemSettingsHandler.Register(v2)
		entityCatalogHandler.Register(v2)
		containerHandler.Register(v2)
		backupHandler.Register(v2)
		distributedBackupHandler.Register(v2)
		saveImportHandler.Register(v2)
		configurationHandler.Register(v2)
		modHandler.Register(v2)
		modPublicationHandler.Register(v2)
		modUpdateHandler.Register(v2)
		gameUpdateHandler.Register(v2)
		luajitHandler.Register(v2)
		gameInstallationHandler.Register(v2)
		worldMapHandler.Register(v2)
	}

	if ownsDatabase {
		hooks.final = append(hooks.final, models.CloseDB)
	}
	completed = true
	application := newApplication(router, hooks)
	application.settings, application.config = systemSettingsService, config
	application.prepareReload = func(ctx context.Context) (func(), error) {
		resume, err := jobService.PauseIfIdle()
		if err != nil {
			return nil, systemsettings.ErrRuntimeBusy
		}
		if embeddedFleetMember != nil {
			resumeMember, memberErr := embeddedFleetMember.PauseIfIdle()
			if memberErr != nil {
				resume()
				return nil, systemsettings.ErrRuntimeBusy
			}
			resumeJobs := resume
			resume = func() { resumeMember(); resumeJobs() }
		}
		if deploymentProfile.LocalExecutorEnabled {
			localRooms, inspectErr := roomCatalog.List()
			if inspectErr != nil {
				resume()
				return nil, inspectErr
			}
			for _, room := range localRooms {
				worlds, inspectErr := roomCatalog.Worlds(room.ID)
				if inspectErr != nil {
					resume()
					return nil, inspectErr
				}
				for _, world := range worlds {
					status, inspectErr := shardControl.Status(ctx, room.DirectoryName, world.DirectoryName)
					if inspectErr != nil || status.SessionExists || status.State != shards.RuntimeStopped && status.State != shards.RuntimeFailed {
						resume()
						return nil, systemsettings.ErrRuntimeBusy
					}
				}
			}
		}
		return resume, nil
	}
	return application, nil
}

func openLocalModManager(config moddistribution.Config) (*moddistribution.Manager, error) {
	manager, err := moddistribution.Open(config)
	if err != nil {
		return nil, fmt.Errorf("initialize local Mod distribution: %w", err)
	}
	return manager, nil
}

func recoverLocalModManager(ctx context.Context, manager *moddistribution.Manager) error {
	recoveryResults, recoveryErr := manager.Recover(ctx)
	if recoveryErr != nil {
		log.Printf("[ModDistribution] local journal recovery failed: %v", recoveryErr)
		for _, result := range recoveryResults {
			if result.Action == "blocked" {
				log.Printf("[ModDistribution] operation=%s recovery-required: %s", result.OperationID, result.Error)
			}
		}
	}
	return recoveryErr
}

func resolveWorkshopPaths(downloadPath, contentPath, appID string) (string, string, error) {
	downloadPath = strings.TrimSpace(downloadPath)
	contentPath = strings.TrimSpace(contentPath)
	appID = strings.TrimSpace(appID)
	if contentPath == "" && downloadPath != "" {
		contentPath = filepath.Join(downloadPath, "steamapps", "workshop", "content", appID)
	}
	if downloadPath == "" && contentPath != "" {
		downloadPath = filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(contentPath))))
	}
	if downloadPath != "" && contentPath != "" {
		expected := filepath.Clean(filepath.Join(downloadPath, "steamapps", "workshop", "content", appID))
		if filepath.Clean(contentPath) != expected {
			return "", "", fmt.Errorf("Workshop path mismatch: WORKSHOP_CONTENT must be %q when WORKSHOP_MOD_PATH is %q", expected, filepath.Clean(downloadPath))
		}
	}
	return downloadPath, contentPath, nil
}

func validateTestAdapters() error {
	environment := os.Getenv("DST_ADMIN_ENV")
	for _, name := range testAdapterEnvironmentVariables {
		driver := os.Getenv(name)
		if driver == "" {
			continue
		}
		if environment != "test" || driver != "memory" {
			return fmt.Errorf("%s is only available as memory in the test environment", name)
		}
	}
	return nil
}
