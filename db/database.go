package db

import (
	"context"
	"database/sql"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/treeverse/lakefs/logging"

	"github.com/jmoiron/sqlx"
)

type TxFunc func(tx Tx) (interface{}, error)

type Rows = sqlx.Rows

type Database interface {
	io.Closer
	Get(dest interface{}, query string, args ...interface{}) error
	Queryx(query string, args ...interface{}) (*Rows, error)
	Exec(query string, args ...interface{}) (rowsAffected int64, err error)
	Transact(fn TxFunc, opts ...TxOpt) (interface{}, error)
	Metadata() (map[string]string, error)
	Stats() sql.DBStats
	WithContext(ctx context.Context) Database
}

type QueryOptions struct {
	logger logging.Logger
	ctx    context.Context
}

type SqlxDatabase struct {
	db           *sqlx.DB
	queryOptions *QueryOptions
}

func NewSqlxDatabase(db *sqlx.DB) *SqlxDatabase {
	return &SqlxDatabase{db: db}
}

func (d *SqlxDatabase) getLogger() logging.Logger {
	if d.queryOptions != nil {
		return d.queryOptions.logger
	}
	return logging.Default()
}

func (d *SqlxDatabase) getContext() context.Context {
	if d.queryOptions != nil {
		return d.queryOptions.ctx
	}
	return context.Background()
}

func (d *SqlxDatabase) WithContext(ctx context.Context) Database {
	return &SqlxDatabase{
		db: d.db,
		queryOptions: &QueryOptions{
			logger: logging.Default().WithContext(ctx),
			ctx:    ctx,
		},
	}
}

func (d *SqlxDatabase) Close() error {
	return d.db.Close()
}

// reportFinish computes the duration since starts and logs a "done" report if that duration is
// long enough.
func (d *SqlxDatabase) reportFinish(err *error, fields logging.Fields, start time.Time) {
	duration := time.Since(start)
	if duration > 100*time.Millisecond {
		logger := d.getLogger().WithFields(fields).WithField("duration", duration)
		if *err != nil {
			logger = logger.WithError(*err)
		}
		logger.Info("database done")
	}
}

func (d *SqlxDatabase) Get(dest interface{}, query string, args ...interface{}) (err error) {
	start := time.Now()
	defer d.reportFinish(&err, logging.Fields{
		"type":  "get",
		"query": query,
		"args":  args,
	}, start)
	return d.db.GetContext(d.getContext(), dest, query, args...)
}

func (d *SqlxDatabase) Queryx(query string, args ...interface{}) (rows *Rows, err error) {
	start := time.Now()
	defer d.reportFinish(&err, logging.Fields{
		"type":  "start query",
		"query": query,
		"args":  args,
	}, start)
	return d.db.QueryxContext(d.getContext(), query, args...)
}

func (d *SqlxDatabase) Exec(query string, args ...interface{}) (count int64, err error) {
	start := time.Now()
	defer d.reportFinish(&err, logging.Fields{
		"type":  "exec",
		"query": query,
		"args":  args,
	}, start)
	res, err := d.db.ExecContext(d.getContext(), query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *SqlxDatabase) getTxOptions() *TxOptions {
	options := DefaultTxOptions()
	if d.queryOptions != nil {
		options.logger = d.queryOptions.logger
		options.ctx = d.queryOptions.ctx
	}
	return options
}

func (d *SqlxDatabase) Transact(fn TxFunc, opts ...TxOpt) (interface{}, error) {
	options := d.getTxOptions()
	for _, opt := range opts {
		opt(options)
	}
	var attempt int
	var ret interface{}
	for attempt < SerializationRetryMaxAttempts {
		if attempt > 0 {
			duration := time.Duration(int(SerializationRetryStartInterval) * attempt)
			dbRetriesCount.Inc()
			options.logger.
				WithField("attempt", attempt).
				WithField("sleep_interval", duration).
				Warn("retrying transaction due to serialization error")
			time.Sleep(duration)
		}

		tx, err := d.db.BeginTxx(options.ctx, &sql.TxOptions{
			Isolation: options.isolationLevel,
			ReadOnly:  options.readOnly,
		})
		if err != nil {
			return nil, err
		}
		ret, err = fn(&dbTx{tx: tx, logger: options.logger})
		if err != nil {
			rollbackErr := tx.Rollback()
			if rollbackErr != nil {
				return nil, rollbackErr
			}
			// retry on serialization error
			if IsSerializationError(err) {
				// retry
				attempt++
				continue
			}
			return nil, err
		} else {
			err = tx.Commit()
			if err != nil {
				// retry on serialization error
				if IsSerializationError(err) {
					attempt++
					continue
				}
				// other commit error
				return nil, err
			}
			// committed successfully, we're done
			return ret, nil
		}
	}
	if attempt == SerializationRetryMaxAttempts {
		options.logger.
			WithField("attempt", attempt).
			Warn("transaction failed after max attempts due to serialization error")
	}
	return nil, ErrSerialization
}

func (d *SqlxDatabase) Metadata() (map[string]string, error) {
	metadata := make(map[string]string)
	version, err := d.getVersion()
	if err == nil {
		metadata["postgresql_version"] = version
	}
	auroraVersion, err := d.getAuroraVersion()
	if err == nil {
		metadata["postgresql_aurora_version"] = auroraVersion
	}

	m, err := d.Transact(func(tx Tx) (interface{}, error) {
		// select name,setting from pg_settings
		// where name in ('data_directory', 'rds.extensions', 'TimeZone', 'work_mem')
		type pgSettings struct {
			Name    string `db:"name"`
			Setting string `db:"setting"`
		}
		var pgs []pgSettings
		err = tx.Select(&pgs,
			`SELECT name, setting FROM pg_settings
					WHERE name IN ('data_directory', 'rds.extensions', 'TimeZone', 'work_mem')`)
		if err != nil {
			return nil, err
		}
		settings := make(map[string]string)
		for _, setting := range pgs {
			if setting.Name == "data_directory" {
				isRDS := strings.HasPrefix(setting.Setting, "/rdsdata")
				settings["is_rds"] = strconv.FormatBool(isRDS)
				continue
			}
			settings[setting.Name] = setting.Setting
		}
		return settings, nil
	}, ReadOnly())
	if err != nil {
		return metadata, nil
	}
	// set pgs settings under the metadata with key prefix
	settings := m.(map[string]string)
	for k, v := range settings {
		metadata["postgresql_setting_"+k] = v
	}
	return metadata, nil
}

func (d *SqlxDatabase) getVersion() (string, error) {
	v, err := d.Transact(func(tx Tx) (interface{}, error) {
		type ver struct {
			Version string `db:"version"`
		}
		var v ver
		err := tx.Get(&v, "SELECT version()")
		if err != nil {
			return "", err
		}
		return v.Version, nil
	}, ReadOnly(), WithLogger(logging.Dummy()))
	if err != nil {
		return "", err
	}
	return v.(string), err
}

func (d *SqlxDatabase) getAuroraVersion() (string, error) {
	v, err := d.Transact(func(tx Tx) (interface{}, error) {
		var v string
		err := tx.Get(&v, "SELECT aurora_version()")
		if err != nil {
			return "", err
		}
		return v, nil
	}, ReadOnly(), WithLogger(logging.Dummy()))
	if err != nil {
		return "", err
	}
	return v.(string), err
}

func (d *SqlxDatabase) Stats() sql.DBStats {
	return d.db.Stats()
}
