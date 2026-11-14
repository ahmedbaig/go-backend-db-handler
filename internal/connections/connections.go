package connections

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/ahmedbaig/go-backend-db-handler/internal/appconfig"
	"github.com/ahmedbaig/go-backend-http"
	"github.com/ahmedbaig/go-backend-monitoring"
	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlserver"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Pool struct {
	mu        sync.RWMutex
	conns     map[string]*gorm.DB
	defaults  map[string]string
	drivers   map[string]string
	instances map[string][]string
}

func dialector(c appconfig.Connection) (gorm.Dialector, error) {
	dsn := c.BuildDSN()
	switch c.Driver {
	case "postgres":
		return postgres.Open(dsn), nil
	case "mysql":
		return mysql.Open(dsn), nil
	case "sqlserver":
		return sqlserver.Open(dsn), nil
	case "sqlite":
		return sqlite.Open(dsn), nil
	}
	return nil, fmt.Errorf("unsupported driver %q", c.Driver)
}

func Open(ctx context.Context, cfg appconfig.DB, log *slog.Logger) (*Pool, error) {
	p := &Pool{conns: map[string]*gorm.DB{}, defaults: map[string]string{}, drivers: map[string]string{}, instances: map[string][]string{}}
	for _, c := range cfg.Connections {
		if err := p.add(ctx, c, log); err != nil {
			_ = p.Close()
			return nil, err
		}
	}
	return p, nil
}

func (p *Pool) Add(ctx context.Context, c appconfig.Connection, log *slog.Logger) error {
	return p.add(ctx, c, log)
}

func (p *Pool) add(ctx context.Context, c appconfig.Connection, log *slog.Logger) error {
	d, err := dialector(c)
	if err != nil {
		return err
	}
	slow := c.SlowQuery.Std()
	if slow == 0 {
		slow = 500 * time.Millisecond
	}
	g, err := gorm.Open(d, &gorm.Config{
		TranslateError:         true,
		SkipDefaultTransaction: true,
		PrepareStmt:            c.Driver != "sqlserver",
		Logger: logger.NewSlogLogger(log.With(slog.String("connection", c.Key())), logger.Config{
			SlowThreshold:             slow,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: true,
			ParameterizedQueries:      true,
		}),
	})
	if err != nil {
		return fmt.Errorf("connection %s: %w", c.Key(), err)
	}
	sqlDB, err := g.DB()
	if err != nil {
		return err
	}
	if c.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(c.MaxOpenConns)
	}
	if c.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(c.MaxIdleConns)
	}
	if c.ConnMaxLifetime > 0 {
		sqlDB.SetConnMaxLifetime(c.ConnMaxLifetime.Std())
	}
	if c.ConnMaxIdleTime > 0 {
		sqlDB.SetConnMaxIdleTime(c.ConnMaxIdleTime.Std())
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pctx); err != nil {
		log.Warn("database not reachable at startup", slog.String("connection", c.Key()), slog.Any("error", err))
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns[c.Key()] = g
	p.drivers[c.Database] = c.Driver
	if c.Instance != "" {
		p.instances[c.Database] = append(p.instances[c.Database], c.Instance)
	}
	if _, set := p.defaults[c.Database]; !set || c.Default {
		p.defaults[c.Database] = c.Key()
	}
	log.Info("database connection ready", slog.String("connection", c.Key()), slog.String("driver", c.Driver))
	return nil
}

func (p *Pool) Get(database, instance string) (*gorm.DB, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	key := database
	if instance != "" {
		key = database + "@" + instance
	} else if d, ok := p.defaults[database]; ok {
		key = d
	}
	g, ok := p.conns[key]
	if !ok {
		return nil, httpx.E(httpx.CodeNotFound).WithMessage("No connection for " + key)
	}
	return g, nil
}

func (p *Pool) Driver(database string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.drivers[database]
}

func (p *Pool) Instances(database string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := append([]string(nil), p.instances[database]...)
	sort.Strings(out)
	return out
}

func (p *Pool) RegisterProbes(pr *monitoring.Prober) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var errs []error
	for key, g := range p.conns {
		g := g
		errs = append(errs, pr.AddProbe("db:"+key, func(ctx context.Context) error {
			s, err := g.DB()
			if err != nil {
				return err
			}
			return s.PingContext(ctx)
		}, monitoring.Interval(15*time.Second), monitoring.Timeout(5*time.Second)))
	}
	return errors.Join(errs...)
}

func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var errs []error
	for _, g := range p.conns {
		if s, err := g.DB(); err == nil {
			errs = append(errs, s.Close())
		}
	}
	return errors.Join(errs...)
}
