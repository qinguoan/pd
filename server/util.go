// Copyright 2016 PingCAP, Inc.
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

package server

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/pkg/capnslog"
	"github.com/golang/protobuf/proto"
	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/kvproto/pkg/msgpb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/kvproto/pkg/util"
	"golang.org/x/net/context"
)

const (
	requestTimeout  = 10 * time.Second
	slowRequestTime = 1 * time.Second
)

// Version information.
var (
	PDBuildTS = "None"
	PDGitHash = "None"
)

// PrintPDInfo prints the PD version information.
func PrintPDInfo() {
	log.Infof("Welcome to the PD.")
	log.Infof("Version:")
	log.Infof("Git Commit Hash: %s", PDGitHash)
	log.Infof("UTC Build Time:  %s", PDBuildTS)
}

func kvGet(c *clientv3.Client, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	kv := clientv3.NewKV(c)

	start := time.Now()
	ctx, cancel := context.WithTimeout(c.Ctx(), requestTimeout)
	resp, err := kv.Get(ctx, key, opts...)
	cancel()

	if cost := time.Now().Sub(start); cost > slowRequestTime {
		log.Warnf("kv gets %s too slow, resp: %v, err: %v, cost: %s", key, resp, err, cost)
	}

	return resp, errors.Trace(err)
}

// A helper function to get value with key from etcd.
// TODO: return the value revision for outer use.
func getValue(c *clientv3.Client, key string, opts ...clientv3.OpOption) ([]byte, error) {
	resp, err := kvGet(c, key, opts...)
	if err != nil {
		return nil, errors.Trace(err)
	}

	if n := len(resp.Kvs); n == 0 {
		return nil, nil
	} else if n > 1 {
		return nil, errors.Errorf("invalid get value resp %v, must only one", resp.Kvs)
	}

	return resp.Kvs[0].Value, nil
}

// Return boolean to indicate whether the key exists or not.
// TODO: return the value revision for outer use.
func getProtoMsg(c *clientv3.Client, key string, msg proto.Message, opts ...clientv3.OpOption) (bool, error) {
	value, err := getValue(c, key, opts...)
	if err != nil {
		return false, errors.Trace(err)
	}
	if value == nil {
		return false, nil
	}

	if err = proto.Unmarshal(value, msg); err != nil {
		return false, errors.Trace(err)
	}

	return true, nil
}

func bytesToUint64(b []byte) (uint64, error) {
	if len(b) != 8 {
		return 0, errors.Errorf("invalid data, must 8 bytes, but %d", len(b))
	}

	return binary.BigEndian.Uint64(b), nil
}

func uint64ToBytes(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// slowLogTxn wraps etcd transaction and log slow one.
type slowLogTxn struct {
	clientv3.Txn
	cancel context.CancelFunc
}

func newSlowLogTxn(client *clientv3.Client) clientv3.Txn {
	ctx, cancel := context.WithTimeout(client.Ctx(), requestTimeout)
	return &slowLogTxn{
		Txn:    client.Txn(ctx),
		cancel: cancel,
	}
}

func (t *slowLogTxn) If(cs ...clientv3.Cmp) clientv3.Txn {
	return &slowLogTxn{
		Txn:    t.Txn.If(cs...),
		cancel: t.cancel,
	}
}

func (t *slowLogTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	return &slowLogTxn{
		Txn:    t.Txn.Then(ops...),
		cancel: t.cancel,
	}
}

// Commit implements Txn Commit interface.
func (t *slowLogTxn) Commit() (*clientv3.TxnResponse, error) {
	start := time.Now()
	resp, err := t.Txn.Commit()
	t.cancel()

	cost := time.Now().Sub(start)
	if cost > slowRequestTime {
		log.Warnf("txn runs too slow, resp: %v, err: %v, cost: %s", resp, err, cost)
	}
	label := "success"
	if err != nil {
		label = "failed"
	}
	txnCounter.WithLabelValues(label).Inc()
	txnDuration.WithLabelValues(label).Observe(cost.Seconds())

	return resp, errors.Trace(err)
}

// convertName converts variable name to a linux type name.
// Like `AbcDef -> abc_def`.
func convertName(str string) string {
	name := make([]byte, 0, 64)
	for i := 0; i < len(str); i++ {
		if str[i] >= 'A' && str[i] <= 'Z' {
			if i > 0 {
				name = append(name, '_')
			}

			name = append(name, str[i]+'a'-'A')
		} else {
			name = append(name, str[i])
		}
	}

	return string(name)
}

func sliceClone(strs []string) []string {
	data := make([]string, 0, len(strs))
	for _, str := range strs {
		data = append(data, str)
	}

	return data
}

// check whether current etcd is running.
func endpointStatus(c *clientv3.Client, endpoint string) (*clientv3.StatusResponse, error) {
	m := clientv3.NewMaintenance(c)

	start := time.Now()
	ctx, cancel := context.WithTimeout(c.Ctx(), requestTimeout)
	resp, err := m.Status(ctx, endpoint)
	cancel()

	if cost := time.Now().Sub(start); cost > slowRequestTime {
		log.Warnf("check etcd %s status, resp: %v, err: %v, cost: %s", endpoint, resp, err, cost)
	}

	return resp, errors.Trace(err)
}

const (
	maxCheckEtcdRunningCount = 100
	checkEtcdRunningDelay    = 100 * time.Millisecond
)

// check etcd starts ok or not
func waitEtcdStart(c *clientv3.Client, endpoint string) error {
	var err error
	for i := 0; i < maxCheckEtcdRunningCount; i++ {
		// etcd may not start ok, we should wait and check again
		_, err = endpointStatus(c, endpoint)
		if err == nil {
			return nil
		}

		time.Sleep(checkEtcdRunningDelay)
		continue
	}

	return errors.Trace(err)
}

func rpcConnect(addr string) (net.Conn, error) {
	req, err := http.NewRequest("GET", pdRPCPrefix, nil)
	if err != nil {
		return nil, errors.Trace(err)
	}

	urls, err := ParseUrls(addr)
	if err != nil {
		return nil, errors.Trace(err)
	}

	for _, url := range urls {
		var conn net.Conn
		switch url.Scheme {
		// used in tests
		case "unix", "unixs":
			conn, err = net.Dial("unix", url.Host)
		default:
			conn, err = net.Dial("tcp", url.Host)
		}

		if err != nil {
			continue
		}
		err = req.Write(conn)
		if err != nil {
			conn.Close()
			continue
		}
		return conn, nil
	}

	return nil, errors.Errorf("connect to %s failed", addr)
}

func rpcCall(conn net.Conn, reqID uint64, request *pdpb.Request) (*pdpb.Response, error) {
	req := &msgpb.Message{
		MsgType: msgpb.MessageType_PdReq,
		PdReq:   request,
	}
	if err := util.WriteMessage(conn, reqID, req); err != nil {
		return nil, errors.Trace(err)
	}
	resp := &msgpb.Message{}
	respID, err := util.ReadMessage(conn, resp)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if respID != reqID {
		return nil, errors.Errorf("message id mismatch: reqID %d respID %d", reqID, respID)
	}
	return resp.GetPdResp(), nil
}

// RPCRequest sends a request to addr and waits for the response.
// Export for API test.
func RPCRequest(addr string, reqID uint64, request *pdpb.Request) (*pdpb.Response, error) {
	conn, err := rpcConnect(addr)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return rpcCall(conn, reqID, request)
}

type redirectFormatter struct{}

// Format turns capnslog logs to ngaut logs.
// TODO: remove ngaut log caller stack, "util.go:xxx"
func (rf *redirectFormatter) Format(pkg string, level capnslog.LogLevel, depth int, entries ...interface{}) {
	if pkg != "" {
		pkg = fmt.Sprint(pkg, ": ")
	}

	logStr := fmt.Sprint(level.Char(), " | ", pkg, entries)

	switch level {
	case capnslog.CRITICAL:
		log.Fatalf(logStr)
	case capnslog.ERROR:
		log.Errorf(logStr)
	case capnslog.WARNING:
		log.Warningf(logStr)
	case capnslog.NOTICE:
		log.Infof(logStr)
	case capnslog.INFO:
		log.Infof(logStr)
	case capnslog.DEBUG:
		log.Debugf(logStr)
	case capnslog.TRACE:
		log.Debugf(logStr)
	}
}

// Flush only for implementing Formatter.
func (rf *redirectFormatter) Flush() {}

// setLogOutput sets output path for all logs.
func setLogOutput(path string) error {
	// PD log.
	log.SetOutputByName(path)
	log.SetRotateByDay()

	// ETCD log.
	capnslog.SetFormatter(&redirectFormatter{})

	return nil
}

// InitLogger initalizes PD's logger.
func InitLogger(cfg *Config) error {
	log.SetLevelByString(cfg.LogLevel)
	log.SetHighlighting(false)

	// Force redirect etcd log to stderr.
	if len(cfg.LogFile) == 0 {
		capnslog.SetFormatter(capnslog.NewPrettyFormatter(os.Stderr, false))
		return nil
	}

	err := setLogOutput(cfg.LogFile)
	if err != nil {
		return errors.Trace(err)
	}

	return nil
}
