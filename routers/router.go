package routers

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	dstinstall "dont/internal/dstserver"
	"dont/internal/gameupdate"
	"dont/internal/httpapi"
	"dont/internal/jobs"
	"dont/internal/logstream"
	modservice "dont/internal/mods"
	playerapi "dont/internal/players"
	"dont/internal/rooms"
	"dont/internal/shards"
	"dont/internal/structuredlogs"
	"dont/internal/systemsettings"
	"dont/internal/systemstatus"
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

// InitRouter 初始化路由
func InitRouter() (*gin.Engine, error) {
	if err := validateTestAdapters(); err != nil {
		return nil, err
	}
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
	if workshopDownloadPath == "" && workshopContentPath != "" {
		workshopDownloadPath = filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(workshopContentPath))))
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
	jobStore := jobs.NewStore(models.DB(), tablePrefix)
	if err := jobStore.Migrate(); err != nil {
		return nil, err
	}
	jobService, err := jobs.NewService(jobStore, jobs.NewBroker())
	if err != nil {
		return nil, err
	}
	agentStore := agentservice.NewStore(models.DB(), tablePrefix)
	if err := agentStore.Migrate(); err != nil {
		return nil, err
	}
	var agentTransport agentservice.Transport = agentservice.NewLegacyTransport(func() *legacyserver.Server { return controller.AgentServer })
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
	if os.Getenv("DST_ADMIN_ENV") != "test" {
		agentService.StartWatcher(context.Background(), 5*time.Second, jobService.Notify)
	}
	var systemStatusProvider systemstatus.Provider = systemstatus.NewLocalProvider("")
	if driver := os.Getenv("DST_ADMIN_TEST_SYSTEM_STATUS"); driver != "" {
		if os.Getenv("DST_ADMIN_ENV") != "test" || driver != "memory" {
			return nil, fmt.Errorf("DST_ADMIN_TEST_SYSTEM_STATUS is only available as memory in the test environment")
		}
		systemStatusProvider = systemstatus.NewMemoryProvider()
	}
	systemStatusHandler := httpapi.NewSystemStatusHandler(systemstatus.NewService(systemStatusProvider))
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
	} = tmuxControl
	if driver := os.Getenv("DST_ADMIN_TEST_CONTROL"); driver != "" {
		if os.Getenv("DST_ADMIN_ENV") != "test" || driver != "memory" {
			return nil, fmt.Errorf("DST_ADMIN_TEST_CONTROL is only available as memory in the test environment")
		}
		shardControl = shards.NewMemoryControlWithLogRoot(savePath)
	}
	shardOperations := shards.NewOperations(roomService, shardControl)
	roomHandler := httpapi.NewRoomHandler(roomService, shardOperations, jobService)
	jobHandler := httpapi.NewJobHandler(jobService)
	logService, err := logstream.NewService(savePath, roomService)
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
	commandService, err := consoleapi.NewService(roomService, shardControl, commandStore)
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
	backupHandler := httpapi.NewBackupHandler(backupService, jobService)
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
	modHandler := httpapi.NewModHandler(modService, jobService)
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
		playerProbe, err = playerapi.NewLogProbe(savePath, roomService, shardControl)
		if err != nil {
			return nil, err
		}
	}
	playerService, err := playerapi.NewService(roomService, shardControl, shardControl, configurationService, playerStore, playerProbe)
	if err != nil {
		return nil, err
	}
	if os.Getenv("DST_ADMIN_ENV") != "test" {
		playerService.StartBanExpiryScheduler(context.Background(), time.Minute)
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
		worldStateSampler, err = worldstate.NewLogSampler(savePath, roomService, shardControl)
		if err != nil {
			return nil, err
		}
	}
	worldStateService, err := worldstate.NewService(roomService, shardControl, worldStateStore, worldStateSampler)
	if err != nil {
		return nil, err
	}
	worldStateHandler := httpapi.NewWorldStateHandler(worldStateService, jobService)
	automationStore := automation.NewStore(models.DB(), tablePrefix)
	if err := automationStore.Migrate(); err != nil {
		return nil, err
	}
	automationExecutor, err := automation.NewDomainExecutor(shardOperations, backupService, commandService, playerService, structuredLogService, worldStateService)
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
	automationScheduler := automation.NewScheduler(automationStore, automationService)
	if os.Getenv("DST_ADMIN_ENV") != "test" {
		if err := automationScheduler.Start(context.Background()); err != nil {
			return nil, err
		}
	}
	automationHandler := httpapi.NewAutomationHandler(automationService, automationScheduler)
	if os.Getenv("DST_ADMIN_ENV") != "test" {
		backupapi.NewScheduler(backupService, jobService).Start(context.Background())
	}
	gameUpdateStore := gameupdate.NewStore(models.DB(), tablePrefix)
	if err := gameUpdateStore.Migrate(); err != nil {
		return nil, err
	}
	var updateRunner gameupdate.CommandRunner = gameupdate.ExecRunner{}
	var latestChecker gameupdate.LatestChecker = gameupdate.NewSteamVersionChecker()
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
	}
	gameUpdateService, err := gameupdate.NewService(
		gameupdate.Config{ServerPath: serverExecutablePath, SteamCMDPath: steamCMDPath},
		roomService, shardControl, backupService, gameUpdateStore, updateRunner, latestChecker,
	)
	if err != nil {
		return nil, err
	}
	gameUpdateHandler := httpapi.NewGameUpdateHandler(gameUpdateService, jobService)
	worldMapStore := worldmap.NewStore(models.DB(), tablePrefix)
	if err := worldMapStore.Migrate(); err != nil {
		return nil, err
	}
	var mapRenderer worldmap.Renderer = worldmap.NewExecRenderer(mapRendererPath)
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
		jobHandler.Register(v2)
		logHandler.Register(v2)
		structuredLogHandler.Register(v2)
		consoleHandler.Register(v2)
		playerHandler.Register(v2)
		worldStateHandler.Register(v2)
		automationHandler.Register(v2)
		agentHandler.Register(v2)
		systemStatusHandler.Register(v2)
		systemSettingsHandler.Register(v2)
		containerHandler.Register(v2)
		backupHandler.Register(v2)
		configurationHandler.Register(v2)
		modHandler.Register(v2)
		gameUpdateHandler.Register(v2)
		worldMapHandler.Register(v2)
	}

	return router, nil
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
