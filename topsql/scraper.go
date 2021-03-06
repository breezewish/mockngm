package topsql

import (
	"context"
	"crypto/tls"
	"time"

	"github.com/pingcap/kvproto/pkg/resource_usage_agent"
	"github.com/pingcap/log"
	"github.com/pingcap/tipb/go-tipb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/breeswish/mockngm/utils"
)

var (
	dialTimeout = 5 * time.Second
)

type Scraper struct {
	ctx       context.Context
	cancel    context.CancelFunc
	tlsConfig *tls.Config
	component utils.Component
}

func NewScraper(ctx context.Context, component utils.Component, tlsConfig *tls.Config) *Scraper {
	ctx, cancel := context.WithCancel(ctx)

	return &Scraper{
		ctx:       ctx,
		cancel:    cancel,
		tlsConfig: tlsConfig,
		component: component,
	}
}

func (s *Scraper) IsDown() bool {
	select {
	case <-s.ctx.Done():
		return true
	default:
		return false
	}
}

func (s *Scraper) Close() {
	s.cancel()
}

func (s *Scraper) Run() {
	log.Info("Starting Top SQL scraping", zap.Stringer("target", s.component))
	switch s.component.Kind {
	case utils.ComponentTiDB:
		s.scrapeTiDB()
	case utils.ComponentTiKV:
		s.scrapeTiKV()
	default:
		panic("unexpected scrape target")
	}
}

func (s *Scraper) scrapeTiDB() {
	bo := newBackoffScrape(s.ctx, s.tlsConfig, s.component.Addr, s.component)
	defer bo.close()

	lastLog := time.Now()
	lastSuppressed := 0

	for {
		record := bo.scrapeTiDBRecord()
		if record == nil {
			return
		}

		lastSuppressed++
		if time.Since(lastLog) > time.Second {
			log.Info("Received Top SQL record", zap.Int("records", lastSuppressed), zap.Stringer("target", s.component))
			lastLog = time.Now()
			lastSuppressed = 0
		}
	}
}

func (s *Scraper) scrapeTiKV() {
	bo := newBackoffScrape(s.ctx, s.tlsConfig, s.component.Addr, s.component)
	defer bo.close()

	lastLog := time.Now()
	lastSuppressed := 0

	for {
		record := bo.scrapeTiKVRecord()
		if record == nil {
			return
		}
		
		lastSuppressed++
		if time.Since(lastLog) > time.Second {
			log.Info("Received Top SQL record", zap.Int("records", lastSuppressed), zap.Stringer("target", s.component))
			lastLog = time.Now()
			lastSuppressed = 0
		}
	}
}

func dial(ctx context.Context, tlsConfig *tls.Config, addr string) (*grpc.ClientConn, error) {
	var tlsOption grpc.DialOption
	if tlsConfig == nil {
		tlsOption = grpc.WithInsecure()
	} else {
		tlsOption = grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig))
	}

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	return grpc.DialContext(
		dialCtx,
		addr,
		tlsOption,
		grpc.WithBlock(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:    10 * time.Second,
			Timeout: 3 * time.Second,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  100 * time.Millisecond, // Default was 1s.
				Multiplier: 1.6,                    // Default
				Jitter:     0.2,                    // Default
				MaxDelay:   3 * time.Second,        // Default was 120s.
			},
		}),
	)
}

type backoffScrape struct {
	ctx       context.Context
	tlsCfg    *tls.Config
	address   string
	component utils.Component

	conn   *grpc.ClientConn
	client interface{}
	stream interface{}

	firstWaitTime time.Duration
	maxRetryTimes uint
}

func newBackoffScrape(ctx context.Context, tlsCfg *tls.Config, address string, component utils.Component) *backoffScrape {
	return &backoffScrape{
		ctx:       ctx,
		tlsCfg:    tlsCfg,
		address:   address,
		component: component,

		firstWaitTime: 2 * time.Second,
		maxRetryTimes: 8,
	}
}

func (bo *backoffScrape) scrapeTiDBRecord() *tipb.TopSQLSubResponse {
	if record := bo.scrape(); record != nil {
		if res, ok := record.(*tipb.TopSQLSubResponse); ok {
			return res
		}
	}
	return nil
}

func (bo *backoffScrape) scrapeTiKVRecord() *resource_usage_agent.ResourceUsageRecord {
	if record := bo.scrape(); record != nil {
		if res, ok := record.(*resource_usage_agent.ResourceUsageRecord); ok {
			return res
		}
	}
	return nil
}

func (bo *backoffScrape) scrape() interface{} {
	if bo.stream != nil {
		switch s := bo.stream.(type) {
		case tipb.TopSQLPubSub_SubscribeClient:
			if record, _ := s.Recv(); record != nil {
				return record
			}
		case resource_usage_agent.ResourceMeteringPubSub_SubscribeClient:
			if record, _ := s.Recv(); record != nil {
				return record
			}
		}
	}

	return bo.backoffScrape()
}

func (bo *backoffScrape) backoffScrape() (record interface{}) {
	utils.WithRetryBackoff(bo.ctx, bo.maxRetryTimes, bo.firstWaitTime, func(retried uint) bool {
		if bo.conn != nil {
			_ = bo.conn.Close()
			bo.conn = nil
			bo.client = nil
			bo.stream = nil
		}

		conn, err := dial(bo.ctx, bo.tlsCfg, bo.address)
		if err != nil {
			log.Warn("Failed to dial Top SQL scrape target", zap.Stringer("target", bo.component), zap.Error(err))
			return false
		}

		bo.conn = conn
		switch bo.component.Kind {
		case utils.ComponentTiDB:
			client := tipb.NewTopSQLPubSubClient(conn)
			bo.client = client
			stream, err := client.Subscribe(bo.ctx, &tipb.TopSQLSubRequest{})
			if err != nil {
				log.Warn("Failed to call Top SQL Subscribe", zap.Stringer("target", bo.component), zap.Error(err))
				return false
			}
			bo.stream = stream
			record, err = stream.Recv()
			if err != nil {
				log.Warn("Failed to call Top SQL Subscribe", zap.Stringer("target", bo.component), zap.Error(err))
				return false
			}

			return true

		case utils.ComponentTiKV:
			client := resource_usage_agent.NewResourceMeteringPubSubClient(conn)
			bo.client = client
			stream, err := client.Subscribe(bo.ctx, &resource_usage_agent.ResourceMeteringRequest{})
			if err != nil {
				log.Warn("Failed to call Top SQL Subscribe", zap.Stringer("target", bo.component), zap.Error(err))
				return false
			}
			bo.stream = stream
			record, err = stream.Recv()
			if err != nil {
				log.Warn("Failed to call Top SQL Subscribe", zap.Stringer("target", bo.component), zap.Error(err))
				return false
			}

			return true
		default:
			return true
		}
	})

	return
}

func (bo *backoffScrape) close() {
	if bo.conn != nil {
		_ = bo.conn.Close()
		bo.conn = nil
		bo.client = nil
		bo.stream = nil
	}
}
