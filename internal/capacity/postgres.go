package capacity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// PGAccounts coordinates account slots through PostgreSQL session locks. A
// lease row reports each held slot; both the session lock and any unexpired prior lease guard admission.
// An expired row alone NEVER permits a second live holder. PostgreSQL closes an
// idle session after TTL, including a paused node, and releases its locks.
// A dead node's unexpired row remains reserved until TTL, covering reconnects.
// No new schema: capacity_leases has shipped since 0.1.0.
type PGAccounts struct {
	db      *sql.DB
	node    string
	ttl     time.Duration
	retryAt atomic.Int64
}

func NewPGAccounts(db *sql.DB, node string) *PGAccounts {
	return &PGAccounts{db: db, node: node, ttl: 15 * time.Second}
}

const accountQueryTimeout = 500 * time.Millisecond

// accountLease owns one dedicated connection until the upstream has finished.
// A failed renewal cancels that upstream before the server's idle-session expiry.
type accountLease struct {
	owner            *PGAccounts
	conn             *sql.Conn
	key              int64
	id               string
	stop, done, lost chan struct{}
	once             sync.Once
}

func accountLockKey(group string, slot int) int64 {
	sum := sha256.Sum256([]byte("dorang.account.slot.v1\x00" + group + "\x00" + strconv.Itoa(slot)))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// try is bounded and never waits for another account holder. The broker calls
// it only after local admission, and releases all local axes on refusal.
func (p *PGAccounts) try(group string, limit int) (lease *accountLease, err error) {
	if time.Now().UnixNano() < p.retryAt.Load() {
		return nil, sql.ErrConnDone
	}
	defer func() {
		if err != nil {
			p.retryAt.Store(time.Now().Add(time.Second).UnixNano())
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), accountQueryTimeout)
	defer cancel()
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	for slot := 0; slot < limit; slot++ {
		key := accountLockKey(group, slot)
		var held bool
		if err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&held); err != nil {
			discardAccountConn(conn)
			return nil, err
		}
		if !held {
			continue
		}
		// A live lease survives a database restart or a lost TCP connection.
		// Do not recycle that slot before its owner has had time to cancel the
		// upstream. Normal release deletes it; dead owners are bounded by TTL.
		var occupied bool
		err = conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM capacity_leases WHERE axis='account' AND axis_key=$1 AND used=$2 AND expires_at>(extract(epoch from clock_timestamp())*1000000)::bigint)`, group, slot).Scan(&occupied)
		if err != nil {
			discardAccountConn(conn)
			return nil, err
		}
		if occupied {
			var unlocked bool
			if err = conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&unlocked); err != nil || !unlocked {
				discardAccountConn(conn)
				return nil, sql.ErrConnDone
			}
			continue
		}
		var token [16]byte
		if _, err = rand.Read(token[:]); err != nil {
			discardAccountConn(conn)
			return nil, err
		}
		l := &accountLease{owner: p, conn: conn, key: key, id: "account:" + hex.EncodeToString(token[:]), stop: make(chan struct{}), done: make(chan struct{}), lost: make(chan struct{})}
		// This setting belongs only to the dedicated slot connection and is reset
		// before it can return to the pool. Database time avoids node clock skew.
		if _, err = conn.ExecContext(ctx, "SELECT set_config('idle_session_timeout',$1,false)", strconv.FormatInt(p.ttl.Milliseconds(), 10)); err != nil {
			discardAccountConn(conn)
			return nil, err
		}
		tx, e := conn.BeginTx(ctx, nil)
		if e != nil {
			discardAccountConn(conn)
			return nil, e
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM capacity_leases WHERE axis='account' AND axis_key=$1 AND used=$2", group, slot)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO capacity_leases(id,node_id,axis,axis_key,amount,used,acquired_at,expires_at)
   VALUES($1,$2,'account',$3,1,$4,(extract(epoch from clock_timestamp())*1000000)::bigint,
   (extract(epoch from clock_timestamp())*1000000)::bigint+$5)`, l.id, p.node, group, slot, p.ttl.Microseconds())
		}
		if err != nil {
			_ = tx.Rollback()
			discardAccountConn(conn)
			return nil, err
		}
		if err = tx.Commit(); err != nil {
			discardAccountConn(conn)
			return nil, err
		}
		go l.renew()
		return l, nil
	}
	_ = conn.Close()
	return nil, nil
}

func discardAccountConn(conn *sql.Conn) {
	// Never put a connection with unknown session-lock state back into the pool.
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

func (l *accountLease) renew() {
	defer close(l.done)
	ticker := time.NewTicker(l.owner.ttl / 4)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), accountQueryTimeout)
			res, err := l.conn.ExecContext(ctx, `UPDATE capacity_leases SET expires_at=(extract(epoch from clock_timestamp())*1000000)::bigint+$1 WHERE id=$2`, l.owner.ttl.Microseconds(), l.id)
			cancel()
			if err == nil {
				var n int64
				n, err = res.RowsAffected()
				if n != 1 {
					err = sql.ErrNoRows
				}
			}
			if err != nil {
				close(l.lost)
				return
			}
		}
	}
}

func (l *accountLease) release() {
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		ctx, cancel := context.WithTimeout(context.Background(), accountQueryTimeout)
		defer cancel()
		_, err := l.conn.ExecContext(ctx, "DELETE FROM capacity_leases WHERE id=$1", l.id)
		if err == nil {
			_, err = l.conn.ExecContext(ctx, "RESET idle_session_timeout")
		}
		var unlocked bool
		if err == nil {
			err = l.conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", l.key).Scan(&unlocked)
		}
		if err != nil || !unlocked {
			discardAccountConn(l.conn)
		} else {
			_ = l.conn.Close()
		}
	})
}

// Context cancels a generation if its shared slot connection cannot be renewed.
// The caller must cancel after the backend has closed its response body.
func (r *Reservation) Context(parent context.Context) (context.Context, context.CancelFunc) {
	if r == nil || r.account == nil {
		return parent, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-r.account.lost:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// Remote releases do not touch this process's queues. Poll only when a waiter
// exists, with the same bounded queues and fairness machinery as local release.
func (b *Broker) sharedWakeLoop() {
	defer b.wg.Done()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-ticker.C:
			b.mu.Lock()
			if !b.closed && len(b.waiters) > 0 {
				b.pass++
				for _, bk := range b.buckets {
					b.serveLocked(bk, 1, b.pass)
				}
				b.drainPendingLocked()
			}
			b.mu.Unlock()
		}
	}
}
