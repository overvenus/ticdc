// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql" // MySQL driver
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/pkg/retry"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// -- Create table
// CREATE TABLE IF NOT EXISTS accounts%d (
// 	id BIGINT PRIMARY KEY,
// 	balance BIGINT NOT NULL,
// 	startts BIGINT NOT NULL
// )
// CREATE TABLE IF NOT EXISTS accounts_seq%d (
// 	id BIGINT PRIMARY KEY,
// 	counter BIGINT NOT NULL,
// 	sequence BIGINT NOT NULL,
// 	startts BIGINT NOT NULL
// )
//
// BEGIN
// -- Add sequential update rows.
// SELECT counter, sequence FROM accounts_seq%d WHERE id = %d FOR UPDATE
// UPDATE accounts_seq%d SET
//   counter = %d,
//   sequence = %d,
//   startts = @@tidb_current_ts
// WHERE id IN (%d, %d)
//
// -- Transcation between accounts.
// SELECT id, balance FROM accounts%d WHERE id IN (%d, %d) FOR UPDATE
// UPDATE accounts%d SET
//   balance = CASE id WHEN %d THEN %d WHEN %d THEN %d END,
//   sequence = %d,
//   startts = @@tidb_current_ts
// WHERE id IN (%d, %d)
// COMMIT
//
// -- Verify sum of balance always be the same.
// SELECT SUM(balance) as total FROM accounts%d
// -- Verify no missing transaction
// SELECT sequence FROM accounts_seq%d ORDER BY sequence

// Test ...
// test.cleanup
// test.prepare
// go { loop { test.workload } }
// go { loop { test.verify } }
type Test interface {
	prepare(ctx context.Context, db *sql.DB, accounts int, tableID int, concurrency int) error
	workload(ctx context.Context, tx *sql.Tx, accounts int, tableID int) error
	verify(ctx context.Context, db *sql.DB, accounts, tableID int, tag string) error
	cleanup(ctx context.Context, db *sql.DB, accounts, tableID int, force bool) bool
}

type sequenceTest struct{}

var _ Test = &sequenceTest{}

func (*sequenceTest) workload(ctx context.Context, tx *sql.Tx, accounts int, tableID int) error {
	const sequenceRowID = 0

	getCounterSeq := fmt.Sprintf("SELECT counter, sequence FROM accounts_seq%d WHERE id = %d FOR UPDATE", tableID, sequenceRowID)
	rows, err := tx.QueryContext(ctx, getCounterSeq)
	if err != nil {
		return errors.Trace(err)
	}
	defer rows.Close()
	var counter, maxSeq int
	rows.Next()
	if err = rows.Scan(&counter, &maxSeq); err != nil {
		return errors.Trace(err)
	}
	rows.Close()

	next := counter % accounts
	if next == sequenceRowID {
		next++
		counter++
	}
	counter++
	addSeqCounter := fmt.Sprintf(`
UPDATE accounts_seq%d SET
  counter = %d,
  sequence = %d,
  startts = @@tidb_current_ts
WHERE id IN (%d, %d)
`, tableID, counter, maxSeq+1, sequenceRowID, next)
	_, err = tx.ExecContext(ctx, addSeqCounter)
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (s *sequenceTest) prepare(ctx context.Context, db *sql.DB, accounts, tableID, concurrency int) error {
	createTable := fmt.Sprintf(`
	CREATE TABLE IF NOT EXISTS accounts_seq%d (
		id BIGINT PRIMARY KEY,
		counter BIGINT NOT NULL,
		sequence BIGINT NOT NULL,
		startts BIGINT NOT NULL
	)`, tableID)
	batchInsertSQLF := func(batchSize, offset int) string {
		args := make([]string, batchSize)
		for j := 0; j < batchSize; j++ {
			args[j] = fmt.Sprintf("(%d, 0, 0, 0)", offset+j)
		}
		return fmt.Sprintf("INSERT IGNORE INTO accounts_seq%d (id, counter, sequence, startts) VALUES %s", tableID, strings.Join(args, ","))
	}

	_ = prepareImpl(ctx, s, createTable, batchInsertSQLF, db, accounts, tableID, concurrency)
	return nil
}

func (*sequenceTest) verify(ctx context.Context, db *sql.DB, accounts, tableID int, tag string) error {
	query := fmt.Sprintf("SELECT sequence FROM accounts_seq%d ORDER BY sequence", tableID)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		log.Warn("select sequence err", zap.String("query", query), zap.Error(err), zap.String("tag", tag))
		return nil
	}
	defer rows.Close()

	var sequence, perviousSequence int
	for rows.Next() {
		err = rows.Scan(&sequence)
		if err != nil {
			log.Warn("select sequence err", zap.String("query", query), zap.Error(err), zap.String("tag", tag))
			return nil
		}
		if perviousSequence != 0 && perviousSequence != sequence && perviousSequence+1 != sequence {
			return errors.Errorf("missing changes sequence %d, perviousSequence %d", sequence, perviousSequence)
		}
		perviousSequence = sequence
	}
	log.Info("sequence verify pass", zap.String("tag", tag))
	return nil
}

//tryDropDB will drop table if data incorrect and panic error likes bad connect.
func (s *sequenceTest) cleanup(ctx context.Context, db *sql.DB, accounts, tableID int, force bool) bool {
	return cleanupImpl(ctx, s, fmt.Sprintf("accounts_seq%d", tableID), db, accounts, tableID, force)
}

type bankTest struct{}

var _ Test = &bankTest{}

func (*bankTest) workload(ctx context.Context, tx *sql.Tx, accounts int, tableID int) error {
	var from, to int
	var (
		fromBalance int
		toBalance   int
		count       int
	)

	for {
		from, to = rand.Intn(accounts), rand.Intn(accounts)
		if from == to {
			continue
		}
		break
	}

	rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT id, balance FROM accounts%d WHERE id IN (%d, %d) FOR UPDATE", tableID, from, to))
	if err != nil {
		return errors.Trace(err)
	}
	defer rows.Close()

	for rows.Next() {
		var id, balance int
		if err = rows.Scan(&id, &balance); err != nil {
			return errors.Trace(err)
		}
		switch id {
		case from:
			fromBalance = balance
		case to:
			toBalance = balance
		default:
			log.Panic(fmt.Sprintf("got unexpected account %d", id))
		}

		count++
	}

	if err = rows.Err(); err != nil {
		return errors.Trace(err)
	}

	if count != 2 {
		log.Panic(fmt.Sprintf("select %d(%d) -> %d(%d) invalid count %d", from, fromBalance, to, toBalance, count))
	}

	amount := rand.Intn(999)

	if fromBalance >= amount {
		update := fmt.Sprintf(`
UPDATE accounts%d SET
  balance = CASE id WHEN %d THEN %d WHEN %d THEN %d END,
  startts = @@tidb_current_ts
WHERE id IN (%d, %d)
`, tableID, to, toBalance+amount, from, fromBalance-amount, from, to)
		_, err = tx.ExecContext(ctx, update)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func (s *bankTest) prepare(ctx context.Context, db *sql.DB, accounts, tableID, concurrency int) error {
	createTable := fmt.Sprintf(`
	CREATE TABLE IF NOT EXISTS accounts%d (
		id BIGINT PRIMARY KEY,
		balance BIGINT NOT NULL,
		startts BIGINT NOT NULL
	)`, tableID)
	batchInsertSQLF := func(batchSize, offset int) string {
		args := make([]string, batchSize)
		for j := 0; j < batchSize; j++ {
			args[j] = fmt.Sprintf("(%d, 1000, 0)", offset+j)
		}
		return fmt.Sprintf("INSERT IGNORE INTO accounts%d (id, balance, startts) VALUES %s", tableID, strings.Join(args, ","))
	}

	_ = prepareImpl(ctx, s, createTable, batchInsertSQLF, db, accounts, tableID, concurrency)
	return nil
}

func (*bankTest) verify(ctx context.Context, db *sql.DB, accounts, tableID int, tag string) error {
	var total int
	var count int

	query := fmt.Sprintf("SELECT SUM(balance) as total FROM accounts%d", tableID)
	err := db.QueryRowContext(ctx, query).Scan(&total)
	if err != nil {
		log.Warn("select sum err", zap.String("query", query), zap.Error(err), zap.String("tag", tag))
		return errors.Trace(err)
	}
	check := accounts * 1000
	if total != check {
		return errors.Errorf("accouts%d total must %d, but got %d", tableID, check, total)
	}

	query = fmt.Sprintf("SELECT COUNT(*) as count FROM accounts%d", tableID)
	err = db.QueryRow(query).Scan(&count)
	if err != nil {
		log.Warn("query failed", zap.String("query", query), zap.Error(err), zap.String("tag", tag))
		return errors.Trace(err)
	}
	if count != accounts {
		return errors.Errorf("account mismatch %d != %d", accounts, count)
	}
	log.Info("bank verify pass", zap.String("tag", tag))
	return nil
}

//tryDropDB will drop table if data incorrect and panic error likes bad connect.
func (s *bankTest) cleanup(ctx context.Context, db *sql.DB, accounts, tableID int, force bool) bool {
	return cleanupImpl(ctx, s, fmt.Sprintf("accounts%d", tableID), db, accounts, tableID, force)
}

func prepareImpl(
	ctx context.Context,
	test Test, createTable string, batchInsertSQLF func(batchSize, offset int) string,
	db *sql.DB, accounts, tableID, concurrency int,
) error {
	isDropped := test.cleanup(ctx, db, accounts, tableID, false)
	if !isDropped {
		return nil
	}

	mustExec(ctx, db, createTable)

	// TODO: fix the error is NumAccounts can't be divided by batchSize.
	// Insert batchSize values in one SQL.
	batchSize := 100
	jobCount := accounts / batchSize
	errg := new(errgroup.Group)

	insertF := func(query string) error {
		_, err := db.ExecContext(ctx, query)
		return err
	}
	ch := make(chan int, jobCount)
	for i := 0; i < concurrency; i++ {
		errg.Go(func() error {
			for {
				startIndex, ok := <-ch
				if !ok {
					return nil
				}

				batchInsertSQL := batchInsertSQLF(batchSize, startIndex)
				start := time.Now()
				err := retry.Run(100*time.Millisecond, 5, func() error { return insertF(batchInsertSQL) })
				if err != nil {
					log.Panic("exec failed", zap.String("query", batchInsertSQL), zap.Error(err))
				}
				log.Info(fmt.Sprintf("insert %d takes %s", batchSize, time.Since(start)), zap.String("query", batchInsertSQL))
			}
		})
	}

	for i := 0; i < jobCount; i++ {
		ch <- i * batchSize
	}
	close(ch)
	_ = errg.Wait()
	return nil
}

func dropTable(ctx context.Context, db *sql.DB, table string) {
	log.Info("drop tables", zap.String("table", table))
	mustExec(ctx, db, fmt.Sprintf("DROP TABLE IF EXISTS %s", table))
}

func cleanupImpl(ctx context.Context, test Test, tableName string, db *sql.DB, accounts, tableID int, force bool) bool {
	if force {
		dropTable(ctx, db, tableName)
		return true
	}

	if !isTableExist(ctx, db, tableName) {
		dropTable(ctx, db, tableName)
		return true
	}

	if err := test.verify(ctx, db, accounts, tableID, "tryDropDB"); err != nil {
		dropTable(ctx, db, tableName)
		return true
	}

	return false
}

func mustExec(ctx context.Context, db *sql.DB, query string) {
	execF := func() error {
		_, err := db.ExecContext(ctx, query)
		return err
	}
	err := retry.Run(100*time.Millisecond, 5, execF)
	if err != nil {
		log.Panic("exec failed", zap.String("query", query), zap.Error(err))
	}
}

func waitTable(ctx context.Context, db *sql.DB, table string) {
	for {
		if isTableExist(ctx, db, table) {
			return
		}
		log.Info("wait table", zap.String("table", table))
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func isTableExist(ctx context.Context, db *sql.DB, table string) bool {
	//if table is not exist ,return true directly
	query := fmt.Sprintf("SHOW TABLES LIKE '%s'", table)
	var t string
	err := db.QueryRowContext(ctx, query).Scan(&t)
	switch {
	case err == sql.ErrNoRows:
		return false
	case err != nil:
		log.Panic("query failed", zap.String("query", query), zap.Error(err))
	}
	return true
}

func openDB(ctx context.Context, dsn string) *sql.DB {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Panic("open db failed", zap.String("dsn", dsn), zap.Error(err))
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		log.Panic("ping db failed", zap.String("dsn", dsn), zap.Error(err))
	}
	return db
}

func run(
	ctx context.Context, upstream, downstream string, accounts, tables int,
	concurrency int, interval time.Duration, testRound int64, cleanupOnly bool,
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	upstreamDB := openDB(ctx, upstream)
	defer upstreamDB.Close()
	downstreamDB := openDB(ctx, downstream)
	defer downstreamDB.Close()
	errg := new(errgroup.Group)
	tests := []Test{&sequenceTest{}, &bankTest{}}

	if cleanupOnly {
		for id := 0; id < tables; id++ {
			tableID := id
			for i := range tests {
				tests[i].cleanup(ctx, upstreamDB, accounts, tableID, true)
				tests[i].cleanup(ctx, downstreamDB, accounts, tableID, true)
			}
		}
		dropTable(ctx, upstreamDB, "finishmark")
		dropTable(ctx, downstreamDB, "finishmark")
		log.Info("cleanup done")
		return
	}

	for id := 0; id < tables; id++ {
		tableID := id
		// Prepare tests
		for i := range tests {
			err := tests[i].prepare(ctx, upstreamDB, accounts, tableID, concurrency)
			if err != nil {
				log.Panic("prepare failed", zap.Error(err))
			}
		}
	}

	// DDL is a strong sync point in TiCDC. Once finishmark table is replicated to downstream
	// all previous DDL and DML are replicated too.
	mustExec(ctx, upstreamDB, `CREATE TABLE IF NOT EXISTS finishmark (foo BIGINT PRIMARY KEY)`)
	waitCtx, waitCancel := context.WithTimeout(ctx, 2*time.Minute)
	waitTable(waitCtx, downstreamDB, "finishmark")
	waitCancel()
	log.Info("all tables synced")

	verifiedRound := int64(0)
	for id := 0; id < tables; id++ {
		tableID := id
		// Verify
		errg.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(interval):
					for i := range tests {
						verifyCtx, verifyCancel := context.WithTimeout(ctx, time.Second*10)
						if err := tests[i].verify(verifyCtx, upstreamDB, accounts, tableID, upstream); err != nil {
							log.Panic("upstream verify fails", zap.Error(err))
						}
						verifyCancel()
						verifyCtx, verifyCancel = context.WithTimeout(ctx, time.Second*10)
						if err := tests[i].verify(verifyCtx, downstreamDB, accounts, tableID, downstream); err != nil {
							log.Panic("downstream verify fails", zap.Error(err))
						}
						verifyCancel()
					}
				}
				if atomic.AddInt64(&verifiedRound, 1) == testRound {
					cancel()
				}
			}
		})

		// Workload
		errg.Go(func() error {
			workload := func(workloadCtx context.Context) error {
				tx, err := upstreamDB.BeginTx(workloadCtx, nil)
				if err != nil {
					return errors.Trace(err)
				}
				defer func() { _ = tx.Rollback() }()

				for i := range tests {
					err := tests[i].workload(workloadCtx, tx, accounts, tableID)
					if err != nil {
						return errors.Trace(err)
					}
				}

				return errors.Trace(tx.Commit())
			}
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				ctx1, cancel1 := context.WithTimeout(ctx, time.Second*10)
				err := workload(ctx1)
				if err != nil && errors.Cause(err) != context.Canceled {
					log.Warn("workload failed", zap.Error(err))
				}
				cancel1()
			}
		})
	}
	_ = errg.Wait()
}
