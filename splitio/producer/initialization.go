package producer

import (
	"errors"
	"fmt"
	"time"

	cfg "github.com/splitio/go-split-commons/v4/conf"
	"github.com/splitio/go-split-commons/v4/dtos"
	"github.com/splitio/go-split-commons/v4/provisional"
	"github.com/splitio/go-split-commons/v4/service/api"
	"github.com/splitio/go-split-commons/v4/telemetry"
	"github.com/splitio/go-toolkit/v5/logging"

	hcAppCommon "github.com/splitio/go-split-commons/v4/healthcheck/application"
	hcServicesCommon "github.com/splitio/go-split-commons/v4/healthcheck/services"
	storageCommon "github.com/splitio/go-split-commons/v4/storage"
	"github.com/splitio/go-split-commons/v4/storage/inmemory"
	"github.com/splitio/go-split-commons/v4/storage/redis"
	"github.com/splitio/go-split-commons/v4/synchronizer"
	"github.com/splitio/go-split-commons/v4/synchronizer/worker/impressionscount"
	"github.com/splitio/go-split-commons/v4/synchronizer/worker/segment"
	"github.com/splitio/go-split-commons/v4/synchronizer/worker/split"
	"github.com/splitio/go-split-commons/v4/tasks"
	"github.com/splitio/split-synchronizer/v4/conf"
	"github.com/splitio/split-synchronizer/v4/splitio/admin"
	adminCommon "github.com/splitio/split-synchronizer/v4/splitio/admin/common"
	"github.com/splitio/split-synchronizer/v4/splitio/common"
	"github.com/splitio/split-synchronizer/v4/splitio/common/impressionlistener"
	ssync "github.com/splitio/split-synchronizer/v4/splitio/common/sync"
	"github.com/splitio/split-synchronizer/v4/splitio/producer/evcalc"
	"github.com/splitio/split-synchronizer/v4/splitio/producer/storage"
	"github.com/splitio/split-synchronizer/v4/splitio/producer/task"
	"github.com/splitio/split-synchronizer/v4/splitio/producer/worker"
	hcApplication "github.com/splitio/split-synchronizer/v4/splitio/provisional/healthcheck/application"
	hcServices "github.com/splitio/split-synchronizer/v4/splitio/provisional/healthcheck/services"
	"github.com/splitio/split-synchronizer/v4/splitio/util"
)

// Start initialize the producer mode
func Start(logger logging.LoggerInterface) error {
	// Getting initial config data
	advanced := conf.ParseAdvancedOptions()
	advanced.EventsBulkSize = conf.Data.EventsPerPost
	advanced.HTTPTimeout = int(conf.Data.HTTPTimeout)
	advanced.ImpressionsBulkSize = conf.Data.ImpressionsPerPost
	advanced.StreamingEnabled = conf.Data.StreamingEnabled
	metadata := util.GetMetadata(false)

	clientKey, err := util.GetClientKey(conf.Data.APIKey)
	if err != nil {
		return common.NewInitError(fmt.Errorf("error parsing client key from provided apikey: %w", err), common.ExitInvalidApikey)
	}

	// Setup fetchers & recorders
	splitAPI := api.NewSplitAPI(conf.Data.APIKey, advanced, logger, metadata)

	// Check if apikey is valid
	if !isValidApikey(splitAPI.SplitFetcher) {
		return common.NewInitError(errors.New("invalid apikey"), common.ExitInvalidApikey)
	}

	// Redis Storages
	redisOptions, err := parseRedisOptions()
	if err != nil {
		return common.NewInitError(fmt.Errorf("error parsing redis config: %w", err), common.ExitRedisInitializationFailed)
	}
	redisClient, err := redis.NewRedisClient(redisOptions, logger)
	if err != nil {
		// THIS BRANCH WILL CURRENTLY NEVER BE REACHED
		// TODO(mredolatti/mmelograno): Currently the commons library panics if the redis server is unreachable.
		// this behaviour should be revisited since this might bring down a client app if called from the sdk
		return common.NewInitError(fmt.Errorf("error instantiating redis client: %w", err), common.ExitRedisInitializationFailed)
	}

	// Instantiating storages
	miscStorage := redis.NewMiscStorage(redisClient, logger)
	err = sanitizeRedis(miscStorage, logger)
	if err != nil {
		return common.NewInitError(fmt.Errorf("error cleaning up redis: %w", err), common.ExitRedisInitializationFailed)
	}

	// Handle dual telemetry:
	// - telemetry generated by split-sync
	// - telemetry generated by sdks and picked up by split-sync
	syncTelemetryStorage, _ := inmemory.NewTelemetryStorage()
	sdkTelemetryStorage := storage.NewRedisTelemetryCosumerclient(redisClient, logger)

	// These storages are forwarded to the dashboard, the sdk-telemetry is irrelevant there
	storages := adminCommon.Storages{
		SplitStorage:          redis.NewSplitStorage(redisClient, logger),
		SegmentStorage:        redis.NewSegmentStorage(redisClient, logger),
		LocalTelemetryStorage: syncTelemetryStorage,
		ImpressionStorage:     redis.NewImpressionStorage(redisClient, dtos.Metadata{}, logger),
		EventStorage:          redis.NewEventsStorage(redisClient, dtos.Metadata{}, logger),
	}

	// Creating Workers and Tasks
	eventEvictionMonitor := evcalc.New(1) // TODO(mredolatti): set the correct thread count
	eventRecorder := worker.NewEventRecorderMultiple(storages.EventStorage, splitAPI.EventRecorder, syncTelemetryStorage, eventEvictionMonitor, logger)

	// Healcheck Monitor
	appMonitor := hcApplication.NewMonitorImp(getAppCountersConfig(storages.SplitStorage), logger)
	appMonitor.Start()

	servicesMonitor := hcServices.NewMonitorImp(getServicesCountersConfig(), logger)
	servicesMonitor.Start()

	workers := synchronizer.Workers{
		SplitFetcher: split.NewSplitFetcher(storages.SplitStorage, splitAPI.SplitFetcher, logger, syncTelemetryStorage, appMonitor),
		SegmentFetcher: segment.NewSegmentFetcher(storages.SplitStorage, storages.SegmentStorage, splitAPI.SegmentFetcher,
			logger, syncTelemetryStorage, appMonitor),
		EventRecorder: eventRecorder,
		TelemetryRecorder: telemetry.NewTelemetrySynchronizer(syncTelemetryStorage, splitAPI.TelemetryRecorder,
			storages.SplitStorage, storages.SegmentStorage, logger, metadata, syncTelemetryStorage),
	}
	splitTasks := synchronizer.SplitTasks{
		SplitSyncTask: tasks.NewFetchSplitsTask(workers.SplitFetcher, conf.Data.SplitsFetchRate, logger),
		SegmentSyncTask: tasks.NewFetchSegmentsTask(workers.SegmentFetcher, conf.Data.SegmentFetchRate,
			advanced.SegmentWorkers, advanced.SegmentQueueSize, logger),
		TelemetrySyncTask: tasks.NewRecordTelemetryTask(workers.TelemetryRecorder, conf.Data.MetricsPostRate, logger),
		EventSyncTask: tasks.NewRecordEventsTasks(workers.EventRecorder, advanced.EventsBulkSize, conf.Data.EventsPostRate,
			logger, conf.Data.EventsThreads),
	}

	impressionEvictionMonitor := evcalc.New(1) // TODO(mredolatti): set the correct thread count
	var impListener impressionlistener.ImpressionBulkListener
	if conf.Data.ImpressionListener.Endpoint != "" {
		// TODO(mredolatti): make the listener queue size configurable
		var err error
		impListener, err = impressionlistener.NewImpressionBulkListener(conf.Data.ImpressionListener.Endpoint, 20, nil)
		if err != nil {
			return common.NewInitError(fmt.Errorf("error instantiating impression listener: %w", err), common.ExitTaskInitialization)
		}

	}

	managerConfig := cfg.ManagerConfig{
		ImpressionsMode: conf.Data.ImpressionsMode,
		OperationMode:   cfg.ProducerSync,
		ListenerEnabled: impListener != nil,
	}

	var impressionsCounter *provisional.ImpressionsCounter
	if conf.Data.ImpressionsMode == cfg.ImpressionsModeOptimized {
		impressionsCounter = provisional.NewImpressionsCounter()
		workers.ImpressionsCountRecorder = impressionscount.NewRecorderSingle(impressionsCounter, splitAPI.ImpressionRecorder, metadata,
			logger, syncTelemetryStorage)
		splitTasks.ImpressionsCountSyncTask = tasks.NewRecordImpressionsCountTask(workers.ImpressionsCountRecorder, logger)
	}
	impressionRecorder, err := worker.NewImpressionRecordMultiple(storages.ImpressionStorage, splitAPI.ImpressionRecorder, impListener,
		syncTelemetryStorage, logger, managerConfig, impressionsCounter, impressionEvictionMonitor)
	if err != nil {
		return common.NewInitError(fmt.Errorf("error instantiating impression recorder: %w", err), common.ExitTaskInitialization)
	}
	splitTasks.ImpressionSyncTask = tasks.NewRecordImpressionsTasks(impressionRecorder, conf.Data.ImpressionsPostRate, logger,
		advanced.ImpressionsBulkSize, conf.Data.ImpressionsThreads)

	sdkTelemetryWorker := worker.NewTelemetryMultiWorker(logger, sdkTelemetryStorage, splitAPI.TelemetryRecorder)
	sdkTelemetryTask := task.NewTelemetrySyncTask(sdkTelemetryWorker, logger, 10)
	syncImpl := ssync.NewSynchronizer(advanced, splitTasks, workers, logger, nil, []tasks.Task{sdkTelemetryTask}, appMonitor)
	managerStatus := make(chan int, 1)
	syncManager, err := synchronizer.NewSynchronizerManager(
		syncImpl,
		logger,
		advanced,
		splitAPI.AuthClient,
		storages.SplitStorage,
		managerStatus,
		syncTelemetryStorage,
		metadata,
		&clientKey,
		appMonitor,
	)

	if err != nil {
		return common.NewInitError(fmt.Errorf("error instantiating sync manager: %w", err), common.ExitTaskInitialization)
	}

	rtm := common.NewRuntime(false, syncManager, logger, conf.Data.Producer.Admin.Title, nil, nil)

	// --------------------------- ADMIN DASHBOARD ------------------------------
	adminServer, err := admin.NewServer(&admin.Options{
		Host:                "0.0.0.0",
		Port:                conf.Data.Producer.Admin.Port,
		Name:                "Split Synchronizer dashboard (producer mode)",
		Proxy:               false,
		Username:            conf.Data.Proxy.AdminUsername,
		Password:            conf.Data.Producer.Admin.Password,
		Logger:              logger,
		Storages:            storages,
		ImpressionsEvCalc:   impressionEvictionMonitor,
		ImpressionsRecorder: impressionRecorder,
		EventRecorder:       eventRecorder,
		EventsEvCalc:        eventEvictionMonitor,
		Runtime:             rtm,
		HcAppMonitor:        appMonitor,
		HcServicesMonitor:   servicesMonitor,
	})
	if err != nil {
		panic(err.Error())
	}
	go adminServer.ListenAndServe()

	// Run Sync Manager
	before := time.Now()
	go syncManager.Start()
	select {
	case status := <-managerStatus:
		switch status {
		case synchronizer.Ready:
			logger.Info("Synchronizer tasks started")
			workers.TelemetryRecorder.SynchronizeConfig(
				telemetry.InitConfig{
					AdvancedConfig: advanced,
					TaskPeriods: cfg.TaskPeriods{
						SplitSync:      conf.Data.SplitsFetchRate,
						SegmentSync:    conf.Data.SegmentFetchRate,
						ImpressionSync: conf.Data.ImpressionsPostRate,
						TelemetrySync:  10, // TODO(mredolatti): Expose this as a config option
					},
					ManagerConfig: managerConfig,
				},
				time.Now().Sub(before).Milliseconds(),
				map[string]int64{conf.Data.APIKey: 1},
				nil,
			)
		case synchronizer.Error:
			logger.Error("Initial synchronization failed. Either split is unreachable or the APIKey is incorrect. Aborting execution.")
			return common.NewInitError(fmt.Errorf("error instantiating sync manager: %w", err), common.ExitTaskInitialization)
		}
	}

	rtm.RegisterShutdownHandler()
	rtm.Block()
	return nil
}

func getAppCountersConfig(storage storageCommon.SplitStorage) []*hcAppCommon.Config {
	var cfgs []*hcAppCommon.Config

	splitsConfig := hcAppCommon.NewApplicationConfig("Splits", hcAppCommon.Splits)
	segmentsConfig := hcAppCommon.NewApplicationConfig("Segments", hcAppCommon.Segments)
	storageConfig := hcAppCommon.NewApplicationConfig("Storage", hcAppCommon.Storage)
	storageConfig.Periodic = true
	storageConfig.MaxErrorsAllowedInPeriod = 2
	storageConfig.Severity = hcAppCommon.Low
	storageConfig.TaskFunc = func(l logging.LoggerInterface, c hcAppCommon.CounterInterface) error {
		_, err := storage.ChangeNumber()
		if err != nil {
			c.NotifyEvent()
			return nil
		}

		c.UpdateLastHit()
		return nil
	}

	cfgs = append(cfgs, splitsConfig, segmentsConfig, storageConfig)

	return cfgs
}

func getServicesCountersConfig() []*hcServicesCommon.Config {
	var cfgs []*hcServicesCommon.Config

	telemetryConfig := hcServicesCommon.NewServicesConfig("Telemetry", "https://telemetry.split-stage.io", "/version")
	authConfig := hcServicesCommon.NewServicesConfig("Auth", "https://auth.split-stage.io", "/version")
	apiConfig := hcServicesCommon.NewServicesConfig("API", "https://sdk.split-stage.io/api", "/version")
	eventsConfig := hcServicesCommon.NewServicesConfig("Events", "https://events.split-stage.io/api", "/version")
	streamingConfig := hcServicesCommon.NewServicesConfig("Streaming", "https://streaming.split.io", "/health")

	return append(cfgs, telemetryConfig, authConfig, apiConfig, eventsConfig, streamingConfig)
}
