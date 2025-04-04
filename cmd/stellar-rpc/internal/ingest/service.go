package ingest

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stellar/go/historyarchive"
	"github.com/stellar/go/ingest"
	backends "github.com/stellar/go/ingest/ledgerbackend"
	supportdb "github.com/stellar/go/support/db"
	"github.com/stellar/go/support/log"
	"github.com/stellar/go/xdr"

	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/daemon/interfaces"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/db"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/feewindow"
	"github.com/stellar/stellar-rpc/cmd/stellar-rpc/internal/util"
)

const (
	ledgerEntryBaselineProgressLogPeriod = 10000
	maxRetries                           = 5
)

var errEmptyArchives = errors.New("cannot start ingestion without history archives, " +
	"wait until first history archives are published")

type Config struct {
	Logger            *log.Entry
	DB                db.ReadWriter
	FeeWindows        *feewindow.FeeWindows
	NetworkPassPhrase string
	Archive           historyarchive.ArchiveInterface
	LedgerBackend     backends.LedgerBackend
	Timeout           time.Duration
	OnIngestionRetry  backoff.Notify
	Daemon            interfaces.Daemon
}

func NewService(cfg Config) *Service {
	service := newService(cfg)
	startService(service, cfg)
	return service
}

func newService(cfg Config) *Service {
	// ingestionDurationMetric is a metric for measuring the latency of ingestion
	ingestionDurationMetric := prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Namespace: cfg.Daemon.MetricsNamespace(), Subsystem: "ingest", Name: "ledger_ingestion_duration_seconds",
		Help:       "ledger ingestion durations, sliding window = 10m",
		Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001}, //nolint:mnd
	},
		[]string{"type"},
	)
	// latestLedgerMetric is a metric for measuring the latest ingested ledger
	latestLedgerMetric := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: cfg.Daemon.MetricsNamespace(), Subsystem: "ingest", Name: "local_latest_ledger",
		Help: "sequence number of the latest ledger ingested by this ingesting instance",
	})

	// ledgerStatsMetric is a metric which measures statistics on all ledger entries ingested by stellar rpc
	ledgerStatsMetric := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: cfg.Daemon.MetricsNamespace(), Subsystem: "ingest", Name: "ledger_stats_total",
			Help: "counters of different ledger stats",
		},
		[]string{"type"},
	)

	cfg.Daemon.MetricsRegistry().MustRegister(
		ingestionDurationMetric,
		latestLedgerMetric,
		ledgerStatsMetric)

	service := &Service{
		logger:            cfg.Logger,
		db:                cfg.DB,
		feeWindows:        cfg.FeeWindows,
		ledgerBackend:     cfg.LedgerBackend,
		networkPassPhrase: cfg.NetworkPassPhrase,
		timeout:           cfg.Timeout,
		metrics: Metrics{
			ingestionDurationMetric: ingestionDurationMetric,
			latestLedgerMetric:      latestLedgerMetric,
			ledgerStatsMetric:       ledgerStatsMetric,
		},
	}

	return service
}

func startService(service *Service, cfg Config) {
	ctx, done := context.WithCancel(context.Background())
	service.done = done
	service.wg.Add(1)
	panicGroup := util.UnrecoverablePanicGroup.Log(cfg.Logger)
	panicGroup.Go(func() {
		defer service.wg.Done()
		// Retry running ingestion every second for 5 seconds.
		constantBackoff := backoff.WithMaxRetries(backoff.NewConstantBackOff(1*time.Second), maxRetries)
		// Don't want to keep retrying if the context gets canceled.
		contextBackoff := backoff.WithContext(constantBackoff, ctx)
		err := backoff.RetryNotify(
			func() error {
				err := service.run(ctx, cfg.Archive)
				if errors.Is(err, errEmptyArchives) {
					// keep retrying until history archives are published
					constantBackoff.Reset()
				}
				return err
			},
			contextBackoff,
			cfg.OnIngestionRetry)
		if err != nil && !errors.Is(err, context.Canceled) {
			service.logger.WithError(err).Fatal("could not run ingestion")
		}
	})
}

type Metrics struct {
	ingestionDurationMetric *prometheus.SummaryVec
	latestLedgerMetric      prometheus.Gauge
	ledgerStatsMetric       *prometheus.CounterVec
}

type Service struct {
	logger            *log.Entry
	db                db.ReadWriter
	feeWindows        *feewindow.FeeWindows
	ledgerBackend     backends.LedgerBackend
	timeout           time.Duration
	networkPassPhrase string
	done              context.CancelFunc
	wg                sync.WaitGroup
	metrics           Metrics
}

func (s *Service) Close() error {
	s.done()
	s.wg.Wait()
	return nil
}

func (s *Service) run(ctx context.Context, archive historyarchive.ArchiveInterface) error {
	// Create a ledger-entry baseline from a checkpoint if it wasn't done before
	// (after that we will be adding deltas from txmeta ledger entry changes)
	nextLedgerSeq, checkPointFillErr, err := s.maybeFillEntriesFromCheckpoint(ctx, archive)
	if err != nil {
		return err
	}

	// Make sure that the checkpoint prefill (if any), happened before starting to apply deltas
	if err := <-checkPointFillErr; err != nil {
		return err
	}

	for ; ; nextLedgerSeq++ {
		if err := s.ingest(ctx, nextLedgerSeq); err != nil {
			return err
		}
	}
}

func (s *Service) maybeFillEntriesFromCheckpoint(ctx context.Context,
	archive historyarchive.ArchiveInterface,
) (uint32, chan error, error) {
	checkPointFillErr := make(chan error, 1)
	// Skip creating a ledger-entry baseline if the DB was initialized
	curLedgerSeq, err := s.db.GetLatestLedgerSequence(ctx)
	if errors.Is(err, db.ErrEmptyDB) {
		var checkpointLedger uint32
		root, rootErr := archive.GetRootHAS()
		if rootErr != nil {
			return 0, checkPointFillErr, rootErr
		}
		if root.CurrentLedger == 0 {
			return 0, checkPointFillErr, errEmptyArchives
		}
		checkpointLedger = root.CurrentLedger

		// DB is empty, let's fill it from the History Archive, using the latest available checkpoint
		// Do it in parallel with the upcoming captive core preparation to save time
		s.logger.Infof("found an empty database, creating ledger-entry baseline from the most recent "+
			"checkpoint (%d). This can take up to 30 minutes, depending on the network", checkpointLedger)
		panicGroup := util.UnrecoverablePanicGroup.Log(s.logger)
		panicGroup.Go(func() {
			checkPointFillErr <- s.fillEntriesFromCheckpoint(ctx, archive, checkpointLedger)
		})
		return checkpointLedger + 1, checkPointFillErr, nil
	} else if err != nil {
		return 0, checkPointFillErr, err
	}
	checkPointFillErr <- nil
	nextLedgerSeq := curLedgerSeq + 1
	prepareRangeCtx, cancelPrepareRange := context.WithTimeout(ctx, s.timeout)
	defer cancelPrepareRange()
	return nextLedgerSeq,
		checkPointFillErr,
		s.ledgerBackend.PrepareRange(prepareRangeCtx, backends.UnboundedRange(nextLedgerSeq))
}

func (s *Service) fillEntriesFromCheckpoint(ctx context.Context, archive historyarchive.ArchiveInterface,
	checkpointLedger uint32,
) error {
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, s.timeout)
	defer cancel()

	reader, err := ingest.NewCheckpointChangeReader(ctx, archive, checkpointLedger)
	if err != nil {
		return err
	}

	tx, err := s.db.NewTx(ctx)
	if err != nil {
		return err
	}

	prepareRangeErr := make(chan error, 1)
	go func() {
		prepareRangeErr <- s.ledgerBackend.PrepareRange(ctx, backends.UnboundedRange(checkpointLedger))
	}()

	transactionCommitted := false
	defer func() {
		if !transactionCommitted {
			// Internally, we might already have rolled back the transaction. We should
			// not generate benign error/warning here in case the transaction was already rolled back.
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, supportdb.ErrAlreadyRolledback) {
				s.logger.WithError(rollbackErr).Warn("could not rollback fillEntriesFromCheckpoint write transactions")
			}
		}
	}()

	if err := s.ingestLedgerEntryChanges(ctx, reader, tx, ledgerEntryBaselineProgressLogPeriod); err != nil {
		return err
	}
	if err := reader.Close(); err != nil {
		return err
	}

	if err := <-prepareRangeErr; err != nil {
		return err
	}
	var ledgerCloseMeta xdr.LedgerCloseMeta
	if ledgerCloseMeta, err = s.ledgerBackend.GetLedger(ctx, checkpointLedger); err != nil {
		return err
	} else if err = reader.VerifyBucketList(ledgerCloseMeta.BucketListHash()); err != nil {
		return err
	}

	s.logger.Info("committing checkpoint ledger entries")
	err = tx.Commit(ledgerCloseMeta)
	transactionCommitted = true
	if err != nil {
		return err
	}

	s.logger.Info("finished checkpoint processing")
	return nil
}

func (s *Service) ingest(ctx context.Context, sequence uint32) error {
	s.logger.Infof("Ingesting ledger %d", sequence)
	ledgerCloseMeta, err := s.ledgerBackend.GetLedger(ctx, sequence)
	if err != nil {
		return err
	}

	startTime := time.Now()
	reader, err := ingest.NewLedgerChangeReaderFromLedgerCloseMeta(s.networkPassPhrase, ledgerCloseMeta)
	if err != nil {
		return err
	}
	tx, err := s.db.NewTx(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil {
			s.logger.WithError(err).Warn("could not rollback ingest write transactions")
		}
	}()

	if err := s.ingestLedgerEntryChanges(ctx, reader, tx, 0); err != nil {
		return err
	}

	if err := reader.Close(); err != nil {
		return err
	}

	// EvictedTemporaryLedgerKeys will include both temporary ledger keys which
	// have been evicted and their corresponding ttl ledger entries
	evictedTempLedgerKeys, err := ledgerCloseMeta.EvictedTemporaryLedgerKeys()
	if err != nil {
		return err
	}
	if err := s.ingestTempLedgerEntryEvictions(ctx, evictedTempLedgerKeys, tx); err != nil {
		return err
	}

	if err := s.ingestLedgerCloseMeta(tx, ledgerCloseMeta); err != nil {
		return err
	}

	if err := tx.Commit(ledgerCloseMeta); err != nil {
		return err
	}
	s.logger.
		WithField("duration", time.Since(startTime).Seconds()).
		Debugf("Ingested ledger %d", sequence)

	s.metrics.ingestionDurationMetric.
		With(prometheus.Labels{"type": "total"}).
		Observe(time.Since(startTime).Seconds())
	s.metrics.latestLedgerMetric.Set(float64(sequence))
	return nil
}

func (s *Service) ingestLedgerCloseMeta(tx db.WriteTx, ledgerCloseMeta xdr.LedgerCloseMeta) error {
	startTime := time.Now()
	if err := tx.LedgerWriter().InsertLedger(ledgerCloseMeta); err != nil {
		return err
	}
	s.metrics.ingestionDurationMetric.
		With(prometheus.Labels{"type": "ledger_close_meta"}).
		Observe(time.Since(startTime).Seconds())

	startTime = time.Now()
	if err := tx.TransactionWriter().InsertTransactions(ledgerCloseMeta); err != nil {
		return err
	}
	s.metrics.ingestionDurationMetric.
		With(prometheus.Labels{"type": "transactions"}).
		Observe(time.Since(startTime).Seconds())

	if err := tx.EventWriter().InsertEvents(ledgerCloseMeta); err != nil {
		return err
	}

	if err := s.feeWindows.IngestFees(ledgerCloseMeta); err != nil {
		return err
	}

	return nil
}
