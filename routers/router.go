package routers

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dont/controller"
	agentservice "dont/internal/agents"
	"dont/internal/announcements"
	"dont/internal/authn"
	"dont/internal/automation"
	backupapi "dont/internal/backups"
	"dont/internal/capabilities"
	"dont/internal/configuration"
	consoleapi "dont/internal/console"
	"dont/internal/containers"
	"dont/internal/distributedbackup"
	"dont/internal/dstruntime"
	dstinstall "dont/internal/dstserver"
	"dont/internal/gameupdate"
	"dont/internal/httpapi"
	"dont/internal/jobs"
	"dont/internal/kubernetesruntime"
	"dont/internal/logstream"
	"dont/internal/modcontrol"
	"dont/internal/moddistribution"
	"dont/internal/modpublication"
	modservice "dont/internal/mods"
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
		if !completed && databaseOpened && manageBackground {
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
	announcementStore := announcements.NewStore(models.DB(), tablePrefix)
	if err := announcementStore.Migrate(); err != nil {
		return nil, err
	}
	announcementService, err := announcements.NewService(announcementStore)
	if err != nil {
		return nil, err
	}
	announcementHandler := httpapi.NewAnnouncementHandler(announcementService)
	roomStore := rooms.NewStore(models.DB(), tablePrefix)
	if err := roomStore.Migrate(); err != nil {
		return nil, err
	}
	savePath := setting.Path("paths", "DST_SAVE_PATH", "DST_ADMIN_SAVE_PATH")
	backupPath := setting.Path("paths", "DST_BACKUP_PATH", "DST_ADMIN_BACKUP_PATH")
	serverPath := setting.Path("paths", "DST_SERVER_PATH", "DST_ADMIN_SERVER_PATH")
	ugcPath := setting.Path("paths", "DST_UGC_PATH", "DST_ADMIN_UGC_PATH")
	steamCMDPath := setting.Path("mod", "STEAM_CMD_PATH", "DST_ADMIN_STEAMCMD_PATH")
	luaFallbackPath := setting.Path("mod", "LUA_SH_PATH", "DST_ADMIN_LUA_PATH")
	workshopContentPath := setting.Path("mod", "WORKSHOP_CONTENT", "DST_ADMIN_WORKSHOP_CONTENT")
	workshopDownloadPath := setting.Path("mod", "WORKSHOP_MOD_PATH", "DST_ADMIN_WORKSHOP_DOWNLOAD")
	steamAPIKey := setting.String("mod", "STEAM_WEB_API_KEY", "DST_ADMIN_STEAM_API_KEY")
	steamAppID := setting.String("mod", "APP_ID", "DST_ADMIN_STEAM_APP_ID")
	luaBinary := setting.String("mod", "LUA_BINARY", "DST_ADMIN_LUA_BINARY")
	pythonBinary := setting.String("mod", "PYTHON_BINARY", "DST_ADMIN_PYTHON_BINARY")
	if steamAppID == "" {
		steamAppID = "322330"
	}
	workshopDownloadPath, workshopContentPath, err = resolveWorkshopPaths(workshopDownloadPath, workshopContentPath, steamAppID)
	if err != nil {
		return nil, err
	}
	mapRendererPath := setting.Path("map", "RENDERER_PATH", "DST_ADMIN_MAP_RENDERER_PATH")
	mapPath := setting.Path("paths", "DST_MAP_PATH", "DST_ADMIN_MAP_PATH")
	if mapPath == "" {
		mapPath = backupPath + string(os.PathSeparator) + "maps"
	}
	serverMode := setting.String("paths", "DST_SERVER_MODE", "DST_ADMIN_SERVER_MODE")
	serverExecutablePath := serverPath
	serverInstallRoot := serverPath
	serverContentRoot := serverPath
	if layout, ok := dstinstall.Resolve(serverPath, serverMode); ok {
		serverExecutablePath = layout.Executable
		serverInstallRoot = layout.InstallRoot
		serverContentRoot = layout.ContentRoot
	}
	roomCatalog, err := rooms.NewCatalog(savePath, roomStore)
	if err != nil {
		return nil, err
	}
	roomService := rooms.NewService(roomCatalog, roomStore)
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
	if backgroundEnabled {
		agentGateway, err = legacyserver.NewServer(&legacyserver.Config{
			KeyFile: setting.ConfigPath, SecurityKey: setting.String("server", "SECURITY_KEY", "DST_ADMIN_AGENT_SECURITY_KEY"),
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
	agentService.ConfigureLocalRuntime(agentservice.RuntimeConfig{
		DisplayName: "本机", SavePath: savePath, BackupPath: backupPath, ServerPath: serverPath,
		UGCPath: ugcPath, SteamCMDPath: steamCMDPath, WorkshopContentPath: workshopContentPath,
		LuaBinary: luaBinary, LuaFallbackPath: luaFallbackPath, ServerMode: serverMode,
	})
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
	topologyService, err := topology.NewService(roomService, agentService, topologyStore)
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
		hooks.workers = append(hooks.workers, func(ctx context.Context) {
			agentService.WatchInventories(ctx, 60*time.Second, jobService.Notify)
		})
	}
	var systemStatusProvider systemstatus.Provider = systemstatus.NewLocalProvider("")
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
	systemStatusHandler := httpapi.NewSystemStatusHandler(systemStatusService)
	var systemSettingsRepository systemsettings.Repository = systemsettings.NewFileRepository(setting.ConfigPath)
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
	tmuxControl, err := shards.NewTmuxControl(shards.TmuxConfig{
		SaveRoot: savePath, UGCDirectory: ugcPath, ServerPath: serverPath, ServerMode: serverMode,
	})
	if err != nil {
		return nil, err
	}
	var shardControl interface {
		shards.Control
		consoleapi.Sender
		Status(context.Context, string, string) (shards.RuntimeStatus, error)
	} = tmuxControl
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
	nativeCPUExecutor, err := runtimecpu.NewNative(runtimecpu.NativeConfig{ServerRoot: trustedServerRoot})
	if err != nil {
		return nil, err
	}
	if err := nativeRuntimeDriver.ConfigureCPU(nativeCPUExecutor); err != nil {
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
			roomProvisionService.RunRecovery(ctx, time.Minute)
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
	runtimeObservabilityHandler, err := httpapi.NewRuntimeObservabilityHandler(runtimeEventService, runtimeOverviewService)
	if err != nil {
		return nil, err
	}
	shardOperations := shards.NewOperations(roomService, shardControl)
	if os.Getenv("DST_ADMIN_ENV") != "test" {
		shardOperations = shards.NewOperations(roomService, shardControl, runtimeManager)
	}
	if err := shardOperations.ConfigureRuntime(topologyService, runtimeDriverRouter, operationLeaseService); err != nil {
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
	runtimeAuditHandler := httpapi.NewRuntimeAuditHandler(runtimeAuditService)
	runtimeHandler := httpapi.NewDSTRuntimeHandler(runtimeManager, roomService, shardOperations, distributedRuntimeBridge)
	jobHandler := httpapi.NewJobHandler(jobService)
	logService, err := logstream.NewService(savePath, roomService, runtimeDriverRouter)
	if err != nil {
		return nil, err
	}
	logHandler := httpapi.NewLogHandler(logService)
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
	commandService, err := consoleapi.NewService(roomService, runtimeDriverRouter, commandStore)
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
	distributedBackupHandler, err := httpapi.NewDistributedBackupHandler(distributedBackupService, jobService)
	if err != nil {
		return nil, err
	}
	if backgroundEnabled {
		hooks.workers = append(hooks.workers, func(ctx context.Context) {
			distributedBackupService.RunRecovery(ctx, time.Minute)
		})
	}
	saveImportStore := saveimport.NewStore(models.DB(), tablePrefix)
	if err := saveImportStore.Migrate(); err != nil {
		return nil, err
	}
	configurationService, err := configuration.NewService(savePath, roomService, backupService)
	if err != nil {
		return nil, err
	}
	configurationHandler := httpapi.NewConfigurationHandler(configurationService, jobService)
	var modMetadata modservice.MetadataProvider = modservice.NewSteamProvider(steamAPIKey, steamAppID)
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
	modService.ConfigureMutationGuard(localMutationGuard)
	modHandler := httpapi.NewModHandler(modService, jobService)
	modPublicationRoot := filepath.Join(backupPath, ".mod-publication")
	localModManager, err := moddistribution.New(moddistribution.Config{
		CacheRoot: filepath.Join(modPublicationRoot, "cache"), StateRoot: filepath.Join(modPublicationRoot, "state"),
		NodeID: "local", ReserveBytes: 16 << 20,
		Installations: []moddistribution.TrustedInstallation{{
			ID: "default", NodeID: "local", ServerPath: serverContentRoot, SavePath: savePath,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize local Mod distribution: %w", err)
	}
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
	modControlService, err := modcontrol.NewService(modSnapshotSource, modService, modPublicationCoordinator)
	if err != nil {
		return nil, fmt.Errorf("initialize Mod publication service: %w", err)
	}
	modPublicationHandler, err := httpapi.NewModPublicationHandler(modControlService, jobService)
	if err != nil {
		return nil, err
	}
	modHandler.ConfigurePlacementReader(modControlService)
	if backgroundEnabled {
		hooks.workers = append(hooks.workers, func(ctx context.Context) {
			modControlService.RunRecovery(ctx, time.Minute)
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
		playerProbe, err = playerapi.NewTelemetryProbe(nativeProbe, distributedRuntimeBridge, fallbackProbe, runtimeDriverRouter)
		if err != nil {
			return nil, err
		}
	}
	playerService, err := playerapi.NewService(roomService, shardOperations, runtimeDriverRouter, configurationService, playerStore, playerProbe, distributedRuntimeBridge)
	if err != nil {
		return nil, err
	}
	if backgroundEnabled {
		hooks.workers = append(hooks.workers, func(ctx context.Context) {
			playerService.RunBanExpiryScheduler(ctx, time.Minute)
		})
	}
	playerHandler := httpapi.NewPlayerHandler(playerService, jobService)
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
		fallbackSampler, samplerErr := worldstate.NewLogSampler(savePath, roomService, shardControl)
		if samplerErr != nil {
			return nil, samplerErr
		}
		worldStateSampler, err = worldstate.NewRuntimeSampler(distributedRuntimeBridge, fallbackSampler, runtimeDriverRouter)
		if err != nil {
			return nil, err
		}
	}
	worldStateService, err := worldstate.NewService(roomService, shardOperations, worldStateStore, worldStateSampler)
	if err != nil {
		return nil, err
	}
	worldStateHandler := httpapi.NewWorldStateHandler(worldStateService, jobService)
	automationStore := automation.NewStore(models.DB(), tablePrefix)
	if err := automationStore.Migrate(); err != nil {
		return nil, err
	}
	automationExecutor, err := automation.NewDomainExecutor(shardOperations, backupService, commandService, playerService, structuredLogService, worldStateService, runtimeAuditService)
	if err != nil {
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
		if _, changed, ensureErr := automationService.EnsureDefaultPlayerRefresh(room.ID); ensureErr != nil {
			return nil, ensureErr
		} else if changed {
			log.Printf("[AutomationDefaults] ensured player refresh for room=%s", room.ID)
		}
		if backgroundEnabled {
			if _, installErr := runtimeManager.InstallRoom(context.Background(), room.ID); installErr != nil {
				log.Printf("[DSTRuntime] initialize room=%s: %v", room.ID, installErr)
			}
		}
	}
	automationScheduler := automation.NewScheduler(automationStore, automationService)
	if backgroundEnabled {
		hooks.start = append(hooks.start, automationScheduler.Start)
		hooks.stop = append(hooks.stop, automationScheduler.Close)
	}
	roomService.SetManagedRoomLifecycle(func(roomID string) {
		_, changed, ensureErr := automationService.EnsureDefaultPlayerRefresh(roomID)
		if ensureErr != nil {
			log.Printf("[RoomLifecycle] initialize automation room=%s: %v", roomID, ensureErr)
			return
		}
		if changed && backgroundEnabled {
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
	if backgroundEnabled {
		roomService.AddManagedRoomLifecycle(func(roomID string) {
			if _, installErr := runtimeManager.InstallRoom(context.Background(), roomID); installErr != nil {
				log.Printf("[DSTRuntime] initialize managed room=%s: %v", roomID, installErr)
			}
		}, nil)
		roomService.AddWorldLifecycle(func(roomID, worldID string) {
			if _, installErr := runtimeManager.InstallWorld(context.Background(), roomID, worldID); installErr != nil {
				log.Printf("[DSTRuntime] initialize world room=%s world=%s: %v", roomID, worldID, installErr)
			}
		})
	}
	saveImportService, err := saveimport.NewService(saveimport.Config{
		SaveRoot: savePath, ImportRoot: filepath.Join(backupPath, ".imports"), WorkshopRoot: workshopContentPath,
	}, saveImportStore, roomService, shardControl, backupService, modService, localMutationGuard)
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
	var latestChecker gameupdate.LatestChecker = gameupdate.NewSteamVersionChecker()
	var officialReleaseChecker gameupdate.OfficialReleaseChecker = gameupdate.NewKleiReleaseChecker()
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
	gameUpdateHandler := httpapi.NewGameUpdateHandler(gameUpdateService, jobService)
	gameReleaseStore := gameupdate.NewReleaseStore(models.DB(), tablePrefix)
	if err := gameReleaseStore.Migrate(); err != nil {
		return nil, err
	}
	gameReleaseSnapshot, err := gameupdate.NewTopologyReleaseSnapshot(roomService, topologyService, agentService)
	if err != nil {
		return nil, fmt.Errorf("initialize game release topology snapshot: %w", err)
	}
	gameReleaseRuntime, err := gameupdate.NewDriverReleaseRuntime(runtimeDriverRouter, gameUpdateService)
	if err != nil {
		return nil, fmt.Errorf("initialize game release runtime: %w", err)
	}
	gameReleaseProtection, err := gameupdate.NewDistributedReleaseProtection(distributedBackupService)
	if err != nil {
		return nil, fmt.Errorf("initialize game release protection: %w", err)
	}
	gameReleasePlanner, err := gameupdate.NewReleasePlanner(gameReleaseSnapshot, gameReleaseRuntime, latestChecker, gameupdate.DefaultUpdateHeadroom)
	if err != nil {
		return nil, fmt.Errorf("initialize game release planner: %w", err)
	}
	gameReleaseCoordinator, err := gameupdate.NewReleaseCoordinator(
		gameReleasePlanner, gameReleaseRuntime, operationLeaseService, gameReleaseProtection, gameReleaseStore,
	)
	if err != nil {
		return nil, fmt.Errorf("initialize game release coordinator: %w", err)
	}
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
	worldMapHandler := httpapi.NewWorldMapHandler(worldMapService, jobService)
	if err := models.RecordMigration(models.CurrentMigrationVersion); err != nil {
		return nil, err
	}
	capabilityConfig := capabilities.Config{
		SavePath: savePath, BackupPath: backupPath, ServerPath: serverPath, ServerMode: serverMode, SteamCMDPath: steamCMDPath,
		LuaBinary: luaBinary, PythonBinary: pythonBinary, LuaFallbackPath: luaFallbackPath,
		MapRendererPath: mapRendererPath, MapPath: mapPath,
	}
	idempotencyStore := httpapi.NewIdempotencyStore(15*time.Minute, 2048)
	router := gin.New()
	if err := router.SetTrustedProxies(nil); err != nil {
		return nil, err
	}

	router.Use(
		gin.Logger(),
		httpapi.RequestContext(),
		httpapi.Recovery(),
		httpapi.SameOrigin(),
	)
	router.NoRoute(httpapi.NotFound)
	if agentGateway != nil {
		router.GET("/agent", gin.WrapH(agentGateway.Handler()))
	}

	api := router.Group("/api", httpapi.AdminIPPolicy(func() string {
		preferences, runtimeErr := systemSettingsService.Runtime()
		if runtimeErr != nil {
			return ""
		}
		return preferences.IPWhitelist
	}), httpapi.RequireSession(authService))
	{
		v2 := api.Group("/v2", idempotencyStore.Middleware())
		v2.Use(httpapi.RuntimeTargetBoundary())
		authHandler.Register(v2.Group("/auth"))
		announcementHandler.Register(v2)
		v2.GET("/system/capabilities", httpapi.CapabilitiesProvider(func() capabilities.Report {
			return capabilities.Probe(capabilityConfig)
		}))
		v2.GET("/system/setup-checks", httpapi.SetupChecks(func() capabilities.Readiness {
			return capabilities.ProbeReadiness(capabilityConfig, func() (int, error) {
				items, err := roomService.List()
				return len(items), err
			})
		}))
		roomHandler.Register(v2)
		runtimeAuditHandler.Register(v2)
		runtimeHandler.Register(v2)
		runtimeObservabilityHandler.Register(v2)
		jobHandler.Register(v2)
		logHandler.Register(v2)
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
		containerHandler.Register(v2)
		backupHandler.Register(v2)
		distributedBackupHandler.Register(v2)
		saveImportHandler.Register(v2)
		configurationHandler.Register(v2)
		modHandler.Register(v2)
		modPublicationHandler.Register(v2)
		gameUpdateHandler.Register(v2)
		worldMapHandler.Register(v2)
	}

	if manageBackground {
		hooks.final = append(hooks.final, models.CloseDB)
	}
	completed = true
	return newApplication(router, hooks), nil
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
