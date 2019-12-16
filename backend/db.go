// Copyright 2016 The kingshard Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package backend

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/flike/kingshard/core/errors"
	"github.com/flike/kingshard/core/golog"
	"github.com/flike/kingshard/mysql"
)

const (
	Up = iota
	Down
	ManualDown
	Unknown

	InitConnCount           = 16
	DefaultMaxConnNum       = 1024
	PingPeroid        int64 = 4
)

type DB struct {
	sync.RWMutex

	addr     string
	user     string
	password string
	db       string
	state    int32

	maxConnNum  int // 总连接数
	InitConnNum int
	idleConns   chan *Conn // 空闲数
	cacheConns  chan *Conn // 缓存数，使用数=总连接数-空闲数-缓存数
	checkConn   *Conn
	lastPing    int64

	pushConnCount int64
	popConnCount  int64
}

func Open(addr string, user string, password string, dbName string, maxConnNum int) (*DB, error) {
	var err error
	db := new(DB)
	db.addr = addr
	db.user = user
	db.password = password
	db.db = dbName

	if 0 < maxConnNum {
		db.maxConnNum = maxConnNum
		if db.maxConnNum < 16 {
			db.InitConnNum = db.maxConnNum
		} else {
			db.InitConnNum = db.maxConnNum / 4
		}
	} else {
		db.maxConnNum = DefaultMaxConnNum
		db.InitConnNum = InitConnCount
	}
	//check connection
	db.checkConn, err = db.newConn()
	if err != nil {
		// db.Close()
		// 返回无连接的db实例，确保配置中有数据库服务有问题，kingshard也同样启动成功
		return db, err
	}

	db.idleConns = make(chan *Conn, db.maxConnNum)
	db.cacheConns = make(chan *Conn, db.maxConnNum)
	atomic.StoreInt32(&(db.state), Unknown)

	for i := 0; i < db.maxConnNum; i++ {
		if i < db.InitConnNum {
			conn, err := db.newConn()
			if err != nil {
				// db.Close()
				// 返回无连接的db实例，确保配置中有数据库服务有问题，kingshard也同样启动成功
				return db, err
			}
			db.cacheConns <- conn
		} else {
			conn := new(Conn)
			db.idleConns <- conn
		}
		atomic.AddInt64(&db.pushConnCount, 1)
	}
	db.SetLastPing()

	return db, nil
}

func (db *DB) newCheckConn(conn *Conn) {
	go func() {
		select {
		case <-conn.checkChannel:
		case <-time.After(time.Second * 60 * 5):
			conn := new(Conn)
			db.idleConns <- conn
			atomic.AddInt64(&db.pushConnCount, 1)
			return
		}
	}()
}

func (db *DB) Addr() string {
	return db.addr
}

func (db *DB) State() string {
	var state string
	switch db.state {
	case Up:
		state = "up"
	case Down, ManualDown:
		state = "down"
	case Unknown:
		state = "unknow"
	}
	return state
}

func (db *DB) ConnCount() (int, int, int64, int64) {
	db.RLock()
	defer db.RUnlock()
	return len(db.idleConns), len(db.cacheConns), db.pushConnCount, db.popConnCount
}

func (db *DB) Close() error {
	db.Lock()
	idleChannel := db.idleConns
	cacheChannel := db.cacheConns
	db.cacheConns = nil
	db.idleConns = nil
	db.Unlock()
	if cacheChannel == nil || idleChannel == nil {
		return nil
	}

	close(cacheChannel)
	for conn := range cacheChannel {
		db.closeConn(conn)
		conn = nil
	}
	close(idleChannel)

	return nil
}

func (db *DB) getConns() (chan *Conn, chan *Conn) {
	db.RLock()
	cacheConns := db.cacheConns
	idleConns := db.idleConns
	db.RUnlock()
	return cacheConns, idleConns
}

func (db *DB) getCacheConns() chan *Conn {
	db.RLock()
	conns := db.cacheConns
	db.RUnlock()
	return conns
}

func (db *DB) getIdleConns() chan *Conn {
	db.RLock()
	conns := db.idleConns
	db.RUnlock()
	return conns
}

func (db *DB) Ping() error {
	var err error
	if db.checkConn == nil {
		db.checkConn, err = db.newConn()
		if err != nil {
			if db.checkConn != nil {
				db.checkConn.Close()
				db.checkConn = nil
			}
			return err
		}
	}
	err = db.checkConn.Ping()
	if err != nil {
		if db.checkConn != nil {
			db.checkConn.Close()
			db.checkConn = nil
		}
		return err
	}
	return nil
}

func (db *DB) newConn() (*Conn, error) {
	co := new(Conn)

	if err := co.Connect(db.addr, db.user, db.password, db.db); err != nil {
		return nil, err
	}

	co.pushTimestamp = time.Now().Unix()
	co.checkChannel = make(chan int64)
	return co, nil
}

func (db *DB) closeConn(co *Conn) error {
	if co != nil {
		co.Close()
		conns := db.getIdleConns()
		if conns != nil {
			select {
			case conns <- co:
				atomic.AddInt64(&db.pushConnCount, 1)
				return nil
			default:
				return nil
			}
		}
	}
	return nil
}

func (db *DB) tryReuse(co *Conn) error {
	var err error

	err = co.Ping()
	if err != nil {
		db.closeConn(co)
		co, err = db.newConn()

		if err != nil {
			// db.Close()
			return err
		}
	}

	//reuse Connection
	if co.IsInTransaction() {
		//we can not reuse a connection in transaction status
		err = co.Rollback()
		if err != nil {
			return err
		}
	}

	if !co.IsAutoCommit() {
		//we can not  reuse a connection not in autocomit
		_, err = co.exec("set autocommit = 1")
		if err != nil {
			return err
		}
	}

	//connection may be set names early
	//we must use default utf8
	if co.GetCharset() != mysql.DEFAULT_CHARSET {
		err = co.SetCharset(mysql.DEFAULT_CHARSET, mysql.DEFAULT_COLLATION_ID)
		if err != nil {
			return err
		}
	}

	return nil
}

func (db *DB) PopConn() (*Conn, error) {
	var co *Conn
	var err error

	cacheConns, idleConns := db.getConns()
	if cacheConns == nil || idleConns == nil {
		return nil, errors.ErrDatabaseClose
	}
	co = db.GetConnFromCache(cacheConns)
	if co == nil {
		co, err = db.GetConnFromIdle(cacheConns, idleConns)
		if err != nil {
			return nil, err
		}
	}

	err = db.tryReuse(co)
	if err != nil {
		db.closeConn(co)
		co = nil
		return nil, err
	}

	atomic.AddInt64(&db.popConnCount, 1)
	// add check conn
	db.newCheckConn(co)
	return co, nil
}

func (db *DB) GetConnFromCache(cacheConns chan *Conn) *Conn {
	var co *Conn
	var err error
	for 0 < len(cacheConns) {
		co = <-cacheConns
		if co != nil && PingPeroid < time.Now().Unix()-co.pushTimestamp {
			err = co.Ping()
			if err != nil {
				db.closeConn(co)
				co = nil
			}
		}
		if co != nil {
			break
		}
	}
	return co
}

func (db *DB) GetConnFromIdle(cacheConns, idleConns chan *Conn) (*Conn, error) {
	var co *Conn
	var err error
	select {
	case co = <-idleConns:
		// 当空闲连接变成0时，如果执行上下线操作，co会是nil，避免空指针报错需要判断
		if co == nil {
			return nil, errors.ErrConnIsNil
		}
		// 这种情况说明有节点突然宕机，这时co的数据会发生变化，根据地址变空做判断，此时需要重新申请链接
		if co.addr == "" {
			co, err = db.newConn()

			if err != nil {
				// db.Close()
				return nil, err
			}
		} else {
			err = co.Connect(db.addr, db.user, db.password, db.db)
			if err != nil {
				golog.Error("db", "GetConnFromIdle", err.Error(), 0,
					"addr", db.addr,
					"db", db.db, "user", db.user,
				)
			}
		}

		if err != nil {
			db.closeConn(co)
			co = nil
			return nil, err
		}
		return co, nil
	case co = <-cacheConns:
		if co == nil {
			return nil, errors.ErrConnIsNil
		}
		if co != nil && PingPeroid < time.Now().Unix()-co.pushTimestamp {
			err = co.Ping()
			if err != nil {
				db.closeConn(co)
				co = nil
				return nil, errors.ErrBadConn
			}
		}
	}
	return co, nil
}

func (db *DB) PushConn(co *Conn, err error) {
	if co == nil {
		return
	}
	conns := db.getCacheConns()
	if conns == nil {
		co.Close()
		return
	}
	if err != nil {
		db.closeConn(co)
		co = nil
		return
	}
	co.pushTimestamp = time.Now().Unix()
	select {
	case conns <- co:
		co.checkChannel <- co.pushTimestamp
		atomic.AddInt64(&db.pushConnCount, 1)
		return
	default:
		db.closeConn(co)
		co = nil
		return
	}
}

type BackendConn struct {
	*Conn
	db *DB
}

func (p *BackendConn) Close() {
	if p != nil && p.Conn != nil {
		if p.Conn.pkgErr != nil {
			p.db.closeConn(p.Conn)
			p.Conn = nil
		} else {
			p.db.PushConn(p.Conn, nil)
		}
		p.Conn = nil
	}
}

func (db *DB) GetConn() (*BackendConn, error) {
	c, err := db.PopConn()
	if err != nil {
		return nil, err
	}
	return &BackendConn{c, db}, nil
}

func (db *DB) SetLastPing() {
	db.lastPing = time.Now().Unix()
}

func (db *DB) GetLastPing() int64 {
	return db.lastPing
}
