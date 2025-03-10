// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package server

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/blobs"
	"github.com/cockroachdb/cockroach/pkg/blobs/blobspb"
	"github.com/cockroachdb/cockroach/pkg/geo/geos"
	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/kv/bulk"
	"github.com/cockroachdb/cockroach/pkg/kv/kvclient/kvcoord"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/protectedts"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/storagebase"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/rpc"
	"github.com/cockroachdb/cockroach/pkg/rpc/nodedialer"
	"github.com/cockroachdb/cockroach/pkg/server/status"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec"
	"github.com/cockroachdb/cockroach/pkg/sql/distsql"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire"
	"github.com/cockroachdb/cockroach/pkg/sql/querycache"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sessiondata"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlutil"
	"github.com/cockroachdb/cockroach/pkg/sql/stats"
	"github.com/cockroachdb/cockroach/pkg/sql/stmtdiagnostics"
	"github.com/cockroachdb/cockroach/pkg/sqlmigrations"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/cloud"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/metric"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/cockroach/pkg/util/netutil"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/errors"
	"github.com/marusama/semaphore"
	"google.golang.org/grpc"
)

type sqlServer struct {
	pgServer         *pgwire.Server
	distSQLServer    *distsql.ServerImpl
	execCfg          *sql.ExecutorConfig
	internalExecutor *sql.InternalExecutor
	leaseMgr         *sql.LeaseManager
	blobService      *blobs.Service
	// sessionRegistry can be queried for info on running SQL sessions. It is
	// shared between the sql.Server and the statusServer.
	sessionRegistry        *sql.SessionRegistry
	jobRegistry            *jobs.Registry
	migMgr                 *sqlmigrations.Manager
	statsRefresher         *stats.Refresher
	temporaryObjectCleaner *sql.TemporaryObjectCleaner
	internalMemMetrics     sql.MemoryMetrics
	adminMemMetrics        sql.MemoryMetrics
	// sqlMemMetrics are used to track memory usage of sql sessions.
	sqlMemMetrics           sql.MemoryMetrics
	stmtDiagnosticsRegistry *stmtdiagnostics.Registry
}

type sqlServerArgs struct {
	*Config
	stopper *stop.Stopper

	// SQL uses the clock to assign timestamps to transactions, among many
	// others.
	clock *hlc.Clock

	// DistSQL uses rpcContext to set up flows. Less centrally, the executor
	// also uses rpcContext in a number of places to learn whether the server
	// is running insecure, and to read the cluster name.
	rpcContext *rpc.Context

	// SQL mostly uses the DistSender "wrapped" under a *kv.DB, but SQL also
	// uses range descriptors and leaseholders, which DistSender maintains,
	// for debugging and DistSQL planning purposes.
	distSender *kvcoord.DistSender
	// The executorConfig depends on the status server.
	// The status server is handed the stmtDiagnosticsRegistry.
	status *statusServer
	// Narrowed down version of *NodeLiveness.
	nodeLiveness interface {
		jobs.NodeLiveness                    // jobs uses this
		IsLive(roachpb.NodeID) (bool, error) // DistSQLPlanner wants this
	}
	// The executorConfig uses the provider.
	protectedtsProvider protectedts.Provider
	// Gossip is relied upon by distSQLCfg (execinfra.ServerConfig), the executor
	// config, the DistSQL planner, the table statistics cache, the statements
	// diagnostics registry, and the lease manager.
	gossip *gossip.Gossip
	// Used by DistSQLConfig and DistSQLPlanner.
	nodeDialer *nodedialer.Dialer
	// To register blob and DistSQL servers.
	grpcServer *grpc.Server
	// Used by executorConfig.
	recorder *status.MetricsRecorder
	// For the temporaryObjectCleaner.
	isMeta1Leaseholder func(hlc.Timestamp) (bool, error)
	// DistSQLCfg holds on to this to check for node CPU utilization in
	// samplerProcessor.
	runtime *status.RuntimeStatSampler

	// SQL uses KV, both for non-DistSQL and DistSQL execution.
	db *kv.DB

	// Various components want to register themselves with metrics.
	registry *metric.Registry

	// KV depends on the internal executor, so we pass a pointer to an empty
	// struct in this configuration, which newSQLServer fills.
	//
	// TODO(tbg): make this less hacky.
	circularInternalExecutor *sql.InternalExecutor // empty initially
	// DistSQL, lease management, and others want to know the node they're on.
	//
	// TODO(tbg): replace this with a method that can refuse to return a result
	// because once we have multi-tenancy, a NodeID will not be available.
	nodeIDContainer *base.NodeIDContainer

	// Used by backup/restore.
	externalStorage        cloud.ExternalStorageFactory
	externalStorageFromURI cloud.ExternalStorageFromURIFactory

	// The protected timestamps KV subsystem depends on this, so it is bound
	// early but only gets filled in newSQLServer.
	jobRegistry *jobs.Registry
}

func newSQLServer(ctx context.Context, cfg sqlServerArgs) (*sqlServer, error) {
	sessionRegistry := cfg.status.sessionRegistry
	execCfg := &sql.ExecutorConfig{}
	var jobAdoptionStopFile string
	for _, spec := range cfg.Stores.Specs {
		if !spec.InMemory && spec.Path != "" {
			jobAdoptionStopFile = filepath.Join(spec.Path, jobs.PreventAdoptionFile)
			break
		}
	}

	// Create blob service for inter-node file sharing.
	blobService, err := blobs.NewBlobService(cfg.Settings.ExternalIODir)
	if err != nil {
		return nil, errors.Wrap(err, "creating blob service")
	}
	blobspb.RegisterBlobServer(cfg.grpcServer, blobService)

	jobRegistry := cfg.jobRegistry

	{
		regLiveness := jobs.NodeLiveness(cfg.nodeLiveness)
		if testingLiveness := cfg.TestingKnobs.RegistryLiveness; testingLiveness != nil {
			regLiveness = testingLiveness.(*jobs.FakeNodeLiveness)
		}
		*jobRegistry = *jobs.MakeRegistry(
			cfg.AmbientCtx,
			cfg.stopper,
			cfg.clock,
			regLiveness,
			cfg.db,
			cfg.circularInternalExecutor,
			cfg.nodeIDContainer,
			cfg.Settings,
			cfg.HistogramWindowInterval(),
			func(opName, user string) (interface{}, func()) {
				// This is a hack to get around a Go package dependency cycle. See comment
				// in sql/jobs/registry.go on planHookMaker.
				return sql.NewInternalPlanner(opName, nil, user, &sql.MemoryMetrics{}, execCfg)
			},
			jobAdoptionStopFile,
		)
	}
	cfg.registry.AddMetricStruct(jobRegistry.MetricsStruct())

	distSQLMetrics := execinfra.MakeDistSQLMetrics(cfg.HistogramWindowInterval())
	cfg.registry.AddMetricStruct(distSQLMetrics)

	// Set up Lease Manager
	var lmKnobs sql.LeaseManagerTestingKnobs
	if leaseManagerTestingKnobs := cfg.TestingKnobs.SQLLeaseManager; leaseManagerTestingKnobs != nil {
		lmKnobs = *leaseManagerTestingKnobs.(*sql.LeaseManagerTestingKnobs)
	}
	leaseMgr := sql.NewLeaseManager(
		cfg.AmbientCtx,
		cfg.nodeIDContainer,
		cfg.db,
		cfg.clock,
		cfg.circularInternalExecutor,
		cfg.Settings,
		lmKnobs,
		cfg.stopper,
		cfg.LeaseManagerConfig,
	)

	// Set up internal memory metrics for use by internal SQL executors.
	internalMemMetrics := sql.MakeMemMetrics("internal", cfg.HistogramWindowInterval())
	cfg.registry.AddMetricStruct(internalMemMetrics)

	// We do not set memory monitors or a noteworthy limit because the children of
	// this monitor will be setting their own noteworthy limits.
	rootSQLMemoryMonitor := mon.MakeMonitor(
		"root",
		mon.MemoryResource,
		nil,           /* curCount */
		nil,           /* maxHist */
		-1,            /* increment: use default increment */
		math.MaxInt64, /* noteworthy */
		cfg.Settings,
	)
	rootSQLMemoryMonitor.Start(context.Background(), nil, mon.MakeStandaloneBudget(cfg.SQLMemoryPoolSize))

	// bulkMemoryMonitor is the parent to all child SQL monitors tracking bulk
	// operations (IMPORT, index backfill). It is itself a child of the
	// ParentMemoryMonitor.
	bulkMemoryMonitor := mon.MakeMonitorInheritWithLimit("bulk-mon", 0 /* limit */, &rootSQLMemoryMonitor)
	bulkMetrics := bulk.MakeBulkMetrics(cfg.HistogramWindowInterval())
	cfg.registry.AddMetricStruct(bulkMetrics)
	bulkMemoryMonitor.SetMetrics(bulkMetrics.CurBytesCount, bulkMetrics.MaxBytesHist)
	bulkMemoryMonitor.Start(context.Background(), &rootSQLMemoryMonitor, mon.BoundAccount{})

	// Set up the DistSQL temp engine.

	useStoreSpec := cfg.Stores.Specs[cfg.TempStorageConfig.SpecIdx]
	tempEngine, tempFS, err := storage.NewTempEngine(ctx, cfg.StorageEngine, cfg.TempStorageConfig, useStoreSpec)
	if err != nil {
		return nil, errors.Wrap(err, "creating temp storage")
	}
	cfg.stopper.AddCloser(tempEngine)
	// Remove temporary directory linked to tempEngine after closing
	// tempEngine.
	cfg.stopper.AddCloser(stop.CloserFn(func() {
		firstStore := cfg.Stores.Specs[cfg.TempStorageConfig.SpecIdx]
		var err error
		if firstStore.InMemory {
			// First store is in-memory so we remove the temp
			// directory directly since there is no record file.
			err = os.RemoveAll(cfg.TempStorageConfig.Path)
		} else {
			// If record file exists, we invoke CleanupTempDirs to
			// also remove the record after the temp directory is
			// removed.
			recordPath := filepath.Join(firstStore.Path, TempDirsRecordFilename)
			err = storage.CleanupTempDirs(recordPath)
		}
		if err != nil {
			log.Errorf(ctx, "could not remove temporary store directory: %v", err.Error())
		}
	}))

	// Set up admin memory metrics for use by admin SQL executors.
	adminMemMetrics := sql.MakeMemMetrics("admin", cfg.HistogramWindowInterval())
	cfg.registry.AddMetricStruct(adminMemMetrics)

	// Set up the DistSQL server.
	distSQLCfg := execinfra.ServerConfig{
		AmbientContext: cfg.AmbientCtx,
		Settings:       cfg.Settings,
		RuntimeStats:   cfg.runtime,
		DB:             cfg.db,
		Executor:       cfg.circularInternalExecutor,
		FlowDB:         cfg.db,
		RPCContext:     cfg.rpcContext,
		Stopper:        cfg.stopper,
		NodeID:         cfg.nodeIDContainer,
		ClusterID:      &cfg.rpcContext.ClusterID,
		ClusterName:    cfg.ClusterName,

		TempStorage:     tempEngine,
		TempStoragePath: cfg.TempStorageConfig.Path,
		TempFS:          tempFS,
		// COCKROACH_VEC_MAX_OPEN_FDS specifies the maximum number of open file
		// descriptors that the vectorized execution engine may have open at any
		// one time. This limit is implemented as a weighted semaphore acquired
		// before opening files.
		VecFDSemaphore: semaphore.New(envutil.EnvOrDefaultInt("COCKROACH_VEC_MAX_OPEN_FDS", colexec.VecMaxOpenFDsLimit)),
		DiskMonitor:    cfg.TempStorageConfig.Mon,

		ParentMemoryMonitor: &rootSQLMemoryMonitor,
		BulkAdder: func(
			ctx context.Context, db *kv.DB, ts hlc.Timestamp, opts storagebase.BulkAdderOptions,
		) (storagebase.BulkAdder, error) {
			// Attach a child memory monitor to enable control over the BulkAdder's
			// memory usage.
			bulkMon := execinfra.NewMonitor(ctx, &bulkMemoryMonitor, fmt.Sprintf("bulk-adder-monitor"))
			return bulk.MakeBulkAdder(ctx, db, cfg.distSender.RangeDescriptorCache(), cfg.Settings, ts, opts, bulkMon)
		},

		Metrics: &distSQLMetrics,

		JobRegistry:  jobRegistry,
		Gossip:       cfg.gossip,
		NodeDialer:   cfg.nodeDialer,
		LeaseManager: leaseMgr,

		ExternalStorage:        cfg.externalStorage,
		ExternalStorageFromURI: cfg.externalStorageFromURI,
	}
	cfg.TempStorageConfig.Mon.SetMetrics(distSQLMetrics.CurDiskBytesCount, distSQLMetrics.MaxDiskBytesHist)
	if distSQLTestingKnobs := cfg.TestingKnobs.DistSQL; distSQLTestingKnobs != nil {
		distSQLCfg.TestingKnobs = *distSQLTestingKnobs.(*execinfra.TestingKnobs)
	}

	distSQLServer := distsql.NewServer(ctx, distSQLCfg)
	execinfrapb.RegisterDistSQLServer(cfg.grpcServer, distSQLServer)

	virtualSchemas, err := sql.NewVirtualSchemaHolder(ctx, cfg.Settings)
	if err != nil {
		return nil, errors.Wrap(err, "creating virtual schema holder")
	}

	// Set up Executor

	var sqlExecutorTestingKnobs sql.ExecutorTestingKnobs
	if k := cfg.TestingKnobs.SQLExecutor; k != nil {
		sqlExecutorTestingKnobs = *k.(*sql.ExecutorTestingKnobs)
	} else {
		sqlExecutorTestingKnobs = sql.ExecutorTestingKnobs{}
	}

	loggerCtx, _ := cfg.stopper.WithCancelOnStop(ctx)

	nodeInfo := sql.NodeInfo{
		AdminURL:  cfg.AdminURL,
		PGURL:     cfg.PGURL,
		ClusterID: cfg.rpcContext.ClusterID.Get,
		NodeID:    cfg.nodeIDContainer,
	}

	*execCfg = sql.ExecutorConfig{
		Settings:                cfg.Settings,
		NodeInfo:                nodeInfo,
		DefaultZoneConfig:       &cfg.DefaultZoneConfig,
		Locality:                cfg.Locality,
		AmbientCtx:              cfg.AmbientCtx,
		DB:                      cfg.db,
		Gossip:                  cfg.gossip,
		MetricsRecorder:         cfg.recorder,
		DistSender:              cfg.distSender,
		RPCContext:              cfg.rpcContext,
		LeaseManager:            leaseMgr,
		Clock:                   cfg.clock,
		DistSQLSrv:              distSQLServer,
		StatusServer:            cfg.status,
		SessionRegistry:         sessionRegistry,
		JobRegistry:             jobRegistry,
		VirtualSchemas:          virtualSchemas,
		HistogramWindowInterval: cfg.HistogramWindowInterval(),
		RangeDescriptorCache:    cfg.distSender.RangeDescriptorCache(),
		LeaseHolderCache:        cfg.distSender.LeaseHolderCache(),
		RoleMemberCache:         &sql.MembershipCache{},
		TestingKnobs:            sqlExecutorTestingKnobs,

		DistSQLPlanner: sql.NewDistSQLPlanner(
			ctx,
			execinfra.Version,
			cfg.Settings,
			// The node descriptor will be set later, once it is initialized.
			roachpb.NodeDescriptor{},
			cfg.rpcContext,
			distSQLServer,
			cfg.distSender,
			cfg.gossip,
			cfg.stopper,
			cfg.nodeLiveness,
			cfg.nodeDialer,
		),

		TableStatsCache: stats.NewTableStatisticsCache(
			cfg.SQLTableStatCacheSize,
			cfg.gossip,
			cfg.db,
			cfg.circularInternalExecutor,
		),

		// Note: don't forget to add the secondary loggers as closers
		// on the Stopper, below.

		ExecLogger: log.NewSecondaryLogger(
			loggerCtx, nil /* dirName */, "sql-exec",
			true /* enableGc */, false /*forceSyncWrites*/, true, /* enableMsgCount */
		),

		// Note: the auth logger uses sync writes because we don't want an
		// attacker to easily "erase their traces" after an attack by
		// crashing the server before it has a chance to write the last
		// few log lines to disk.
		//
		// TODO(knz): We could worry about disk I/O activity incurred by
		// logging here in case a malicious user spams the server with
		// (failing) connection attempts to cause a DoS failure; this
		// would be a good reason to invest into a syslog sink for logs.
		AuthLogger: log.NewSecondaryLogger(
			loggerCtx, nil /* dirName */, "auth",
			true /* enableGc */, true /*forceSyncWrites*/, true, /* enableMsgCount */
		),

		// AuditLogger syncs to disk for the same reason as AuthLogger.
		AuditLogger: log.NewSecondaryLogger(
			loggerCtx, cfg.SQLAuditLogDirName, "sql-audit",
			true /*enableGc*/, true /*forceSyncWrites*/, true, /* enableMsgCount */
		),

		SlowQueryLogger: log.NewSecondaryLogger(
			loggerCtx, nil, "sql-slow",
			true /*enableGc*/, false /*forceSyncWrites*/, true, /* enableMsgCount */
		),

		QueryCache:                 querycache.New(cfg.SQLQueryCacheSize),
		ProtectedTimestampProvider: cfg.protectedtsProvider,
	}

	cfg.stopper.AddCloser(execCfg.ExecLogger)
	cfg.stopper.AddCloser(execCfg.AuditLogger)
	cfg.stopper.AddCloser(execCfg.SlowQueryLogger)
	cfg.stopper.AddCloser(execCfg.AuthLogger)

	if sqlSchemaChangerTestingKnobs := cfg.TestingKnobs.SQLSchemaChanger; sqlSchemaChangerTestingKnobs != nil {
		execCfg.SchemaChangerTestingKnobs = sqlSchemaChangerTestingKnobs.(*sql.SchemaChangerTestingKnobs)
	} else {
		execCfg.SchemaChangerTestingKnobs = new(sql.SchemaChangerTestingKnobs)
	}
	if gcJobTestingKnobs := cfg.TestingKnobs.GCJob; gcJobTestingKnobs != nil {
		execCfg.GCJobTestingKnobs = gcJobTestingKnobs.(*sql.GCJobTestingKnobs)
	} else {
		execCfg.GCJobTestingKnobs = new(sql.GCJobTestingKnobs)
	}
	if distSQLRunTestingKnobs := cfg.TestingKnobs.DistSQL; distSQLRunTestingKnobs != nil {
		execCfg.DistSQLRunTestingKnobs = distSQLRunTestingKnobs.(*execinfra.TestingKnobs)
	} else {
		execCfg.DistSQLRunTestingKnobs = new(execinfra.TestingKnobs)
	}
	if sqlEvalContext := cfg.TestingKnobs.SQLEvalContext; sqlEvalContext != nil {
		execCfg.EvalContextTestingKnobs = *sqlEvalContext.(*tree.EvalContextTestingKnobs)
	}
	if pgwireKnobs := cfg.TestingKnobs.PGWireTestingKnobs; pgwireKnobs != nil {
		execCfg.PGWireTestingKnobs = pgwireKnobs.(*sql.PGWireTestingKnobs)
	}

	statsRefresher := stats.MakeRefresher(
		cfg.Settings,
		cfg.circularInternalExecutor,
		execCfg.TableStatsCache,
		stats.DefaultAsOfTime,
	)
	execCfg.StatsRefresher = statsRefresher

	// Set up internal memory metrics for use by internal SQL executors.
	sqlMemMetrics := sql.MakeMemMetrics("sql", cfg.HistogramWindowInterval())
	cfg.registry.AddMetricStruct(sqlMemMetrics)
	pgServer := pgwire.MakeServer(
		cfg.AmbientCtx,
		cfg.Config.Config,
		cfg.Settings,
		sqlMemMetrics,
		&rootSQLMemoryMonitor,
		cfg.HistogramWindowInterval(),
		execCfg,
	)

	// Now that we have a pgwire.Server (which has a sql.Server), we can close a
	// circular dependency between the rowexec.Server and sql.Server and set
	// SessionBoundInternalExecutorFactory. The same applies for setting a
	// SessionBoundInternalExecutor on the the job registry.
	ieFactory := func(
		ctx context.Context, sessionData *sessiondata.SessionData,
	) sqlutil.InternalExecutor {
		ie := sql.MakeInternalExecutor(
			ctx,
			pgServer.SQLServer,
			sqlMemMetrics,
			cfg.Settings,
		)
		ie.SetSessionData(sessionData)
		return &ie
	}
	distSQLServer.ServerConfig.SessionBoundInternalExecutorFactory = ieFactory
	jobRegistry.SetSessionBoundInternalExecutorFactory(ieFactory)

	distSQLServer.ServerConfig.ProtectedTimestampProvider = execCfg.ProtectedTimestampProvider

	for _, m := range pgServer.Metrics() {
		cfg.registry.AddMetricStruct(m)
	}
	*cfg.circularInternalExecutor = sql.MakeInternalExecutor(
		ctx, pgServer.SQLServer, internalMemMetrics, cfg.Settings,
	)
	execCfg.InternalExecutor = cfg.circularInternalExecutor
	stmtDiagnosticsRegistry := stmtdiagnostics.NewRegistry(
		cfg.circularInternalExecutor, cfg.db, cfg.gossip, cfg.Settings)
	cfg.status.setStmtDiagnosticsRequester(stmtDiagnosticsRegistry)
	execCfg.StmtDiagnosticsRecorder = stmtDiagnosticsRegistry

	leaseMgr.RefreshLeases(cfg.stopper, cfg.db, cfg.gossip)
	leaseMgr.PeriodicallyRefreshSomeLeases()

	temporaryObjectCleaner := sql.NewTemporaryObjectCleaner(
		cfg.Settings,
		cfg.db,
		cfg.registry,
		distSQLServer.ServerConfig.SessionBoundInternalExecutorFactory,
		cfg.status,
		cfg.isMeta1Leaseholder,
		sqlExecutorTestingKnobs,
	)

	if err := geos.EnsureInit(geos.EnsureInitErrorDisplayPrivate); err != nil {
		log.Infof(ctx, "could not initialize GEOS - geospatial functions may not be available: %v", err)
	}

	return &sqlServer{
		pgServer:                pgServer,
		distSQLServer:           distSQLServer,
		execCfg:                 execCfg,
		internalExecutor:        cfg.circularInternalExecutor,
		leaseMgr:                leaseMgr,
		blobService:             blobService,
		sessionRegistry:         sessionRegistry,
		jobRegistry:             jobRegistry,
		statsRefresher:          statsRefresher,
		temporaryObjectCleaner:  temporaryObjectCleaner,
		internalMemMetrics:      internalMemMetrics,
		adminMemMetrics:         adminMemMetrics,
		sqlMemMetrics:           sqlMemMetrics,
		stmtDiagnosticsRegistry: stmtDiagnosticsRegistry,
	}, nil
}

func (s *sqlServer) start(
	ctx context.Context,
	stopper *stop.Stopper,
	knobs base.TestingKnobs,
	connManager netutil.Server,
	pgL net.Listener,
	socketFile string,
	orphanedLeasesTimeThresholdNanos int64,
) error {
	s.temporaryObjectCleaner.Start(ctx, stopper)
	s.distSQLServer.Start()
	s.pgServer.Start(ctx, stopper)
	if err := s.statsRefresher.Start(ctx, stopper, stats.DefaultRefreshInterval); err != nil {
		return err
	}
	s.stmtDiagnosticsRegistry.Start(ctx, stopper)

	// Before serving SQL requests, we have to make sure the database is
	// in an acceptable form for this version of the software.
	// We have to do this after actually starting up the server to be able to
	// seamlessly use the kv client against other nodes in the cluster.
	var mmKnobs sqlmigrations.MigrationManagerTestingKnobs
	if migrationManagerTestingKnobs := knobs.SQLMigrationManager; migrationManagerTestingKnobs != nil {
		mmKnobs = *migrationManagerTestingKnobs.(*sqlmigrations.MigrationManagerTestingKnobs)
	}
	migrationsExecutor := sql.MakeInternalExecutor(
		ctx, s.pgServer.SQLServer, s.internalMemMetrics, s.execCfg.Settings)
	migrationsExecutor.SetSessionData(
		&sessiondata.SessionData{
			// Migrations need an executor with query distribution turned off. This is
			// because the node crashes if migrations fail to execute, and query
			// distribution introduces more moving parts. Local execution is more
			// robust; for example, the DistSender has retries if it can't connect to
			// another node, but DistSQL doesn't. Also see #44101 for why DistSQL is
			// particularly fragile immediately after a node is started (i.e. the
			// present situation).
			DistSQLMode: sessiondata.DistSQLOff,
		})
	migMgr := sqlmigrations.NewManager(
		stopper,
		s.execCfg.DB,
		&migrationsExecutor,
		s.execCfg.Clock,
		mmKnobs,
		s.execCfg.NodeID.String(), // TODO(tbg): set yet?
		s.execCfg.Settings,
		s.jobRegistry,
	)
	s.migMgr = migMgr // only for testing via TestServer

	if err := s.jobRegistry.Start(
		ctx, stopper, jobs.DefaultCancelInterval, jobs.DefaultAdoptInterval,
	); err != nil {
		return err
	}

	{
		// Run startup migrations (note: these depend on jobs subsystem running).
		var bootstrapVersion roachpb.Version
		if err := s.execCfg.DB.Txn(ctx, func(ctx context.Context, txn *kv.Txn) error {
			return txn.GetProto(ctx, keys.BootstrapVersionKey, &bootstrapVersion)
		}); err != nil {
			return err
		}
		if err := migMgr.EnsureMigrations(ctx, bootstrapVersion); err != nil {
			return errors.Wrap(err, "ensuring SQL migrations")
		}

		log.Infof(ctx, "done ensuring all necessary migrations have run")
	}

	// Start serving SQL clients.
	if err := s.startServeSQL(ctx, stopper, connManager, pgL, socketFile); err != nil {
		return err
	}

	// Start the async migration to upgrade 19.2-style jobs so they can be run by
	// the job registry in 20.1.
	if err := migMgr.StartSchemaChangeJobMigration(ctx); err != nil {
		return err
	}

	// Delete all orphaned table leases created by a prior instance of this
	// node. This also uses SQL.
	s.leaseMgr.DeleteOrphanedLeases(orphanedLeasesTimeThresholdNanos)

	return nil
}
