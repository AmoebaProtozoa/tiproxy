// Copyright 2022 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package backend

import (
	"fmt"
	"strings"
	"testing"

	pnet "github.com/pingcap/TiProxy/pkg/proxy/net"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/stretchr/testify/require"
)

// TestSuite creates a sql client and a sql server which connect to the proxy with TCP connection.
// Both the mocked client and server act like a real client and server: they parse data and
// respond to the proxy.
// We can test various client capabilities, auth-plugins, and TLS configs using the mocked client
// and server. We create an M^N combinations of the possibilities to test.
// We test the correctness by checking whether the data received by the client equals to the data
// sent from the server and vice versa.

const (
	defaultBackendCapability = mysql.ClientLongPassword | mysql.ClientLongFlag |
		mysql.ClientConnectWithDB | mysql.ClientProtocol41 | mysql.ClientSSL |
		mysql.ClientTransactions | mysql.ClientSecureConnection | mysql.ClientFoundRows |
		mysql.ClientMultiStatements | mysql.ClientMultiResults | mysql.ClientLocalFiles |
		mysql.ClientConnectAtts | mysql.ClientPluginAuth | mysql.ClientInteractive
	defaultClientCapability = defaultBackendCapability
)

var (
	mockUsername = "test_user"
	mockDBName   = "test_db"
	mockSalt     = []byte("01234567890123456789")
	mockAuthData = []byte("123456")
	mockToken    = strings.Repeat("t", 512)
	mockCmdStr   = "str"
	mockCmdInt   = 100
	mockCmdByte  = byte(1)
	mockCmdBytes = []byte("01234567890123456789")
)

type testConfig struct {
	clientConfig  *clientConfig
	proxyConfig   *proxyConfig
	backendConfig *backendConfig
}

type cfgOverrider func(config *testConfig)

// Get a combination of M^N configurations from [N][M]cfgOverrider to test.
func getCfgCombinations(cfgs [][]cfgOverrider) [][]cfgOverrider {
	if len(cfgs) == 0 {
		return nil
	}
	cfgOverriders := make([][]cfgOverrider, 0, len(cfgs[0]))
	for _, cfg := range cfgs[0] {
		cfgOverriders = append(cfgOverriders, []cfgOverrider{cfg})
	}
	for _, cfgList := range cfgs[1:] {
		newOverriders := make([][]cfgOverrider, 0, len(cfgOverriders)*len(cfgList))
		// Append the cfg to each of the existing overrider list.
		for _, cfg := range cfgList {
			for _, o := range cfgOverriders {
				newOverrider := make([]cfgOverrider, 0, len(o)+1)
				newOverrider = append(newOverrider, o...)
				newOverrider = append(newOverrider, cfg)
				newOverriders = append(newOverriders, append(o, cfg))
			}
		}
		cfgOverriders = newOverriders
	}
	return cfgOverriders
}

func newTestConfig(overriders ...cfgOverrider) *testConfig {
	cfg := &testConfig{
		clientConfig:  newClientConfig(),
		proxyConfig:   newProxyConfig(),
		backendConfig: newBackendConfig(),
	}
	for _, overrider := range overriders {
		if overrider != nil {
			overrider(cfg)
		}
	}
	return cfg
}

type testSuite struct {
	tc *tcpConnSuite
	mb *mockBackend
	mp *mockProxy
	mc *mockClient
}

type checker func(t *testing.T, ts *testSuite)

func newTestSuite(t *testing.T, tc *tcpConnSuite, overriders ...cfgOverrider) (*testSuite, func()) {
	ts := &testSuite{}
	cfg := newTestConfig(append(overriders, func(config *testConfig) {
		config.backendConfig.tlsConfig = tc.backendTLSConfig
		config.proxyConfig.backendTLSConfig = tc.clientTLSConfig
		config.proxyConfig.frontendTLSConfig = tc.backendTLSConfig
		config.clientConfig.tlsConfig = tc.clientTLSConfig
	})...)
	ts.mb = newMockBackend(cfg.backendConfig)
	ts.mp = newMockProxy(cfg.proxyConfig)
	ts.mc = newMockClient(cfg.clientConfig)
	ts.tc = tc
	clean := tc.newConn(t)
	return ts, clean
}

func (ts *testSuite) setConfig(overriders ...cfgOverrider) {
	cfg := newTestConfig(overriders...)
	ts.mb.backendConfig = cfg.backendConfig
	ts.mp.proxyConfig = cfg.proxyConfig
	ts.mc.clientConfig = cfg.clientConfig
}

func (ts *testSuite) changeDB(db string) {
	ts.mc.dbName = db
	ts.mp.auth.updateCurrentDB(db)
}

func (ts *testSuite) changeUser(username, db string) {
	ts.mc.username = username
	ts.mc.dbName = db
	ts.mp.auth.changeUser(username, db)
}

func (ts *testSuite) runAndCheck(t *testing.T, c checker, clientRunner, backendRunner func(*pnet.PacketIO) error,
	proxyRunner func(*pnet.PacketIO, *pnet.PacketIO) error) {
	ts.mc.err, ts.mb.err, ts.mp.err = ts.tc.run(t, clientRunner, backendRunner, proxyRunner)
	if c == nil {
		require.NoError(t, ts.mc.err)
		require.NoError(t, ts.mb.err)
		if ts.mb.err != nil {
			require.True(t, IsMySQLError(ts.mp.err))
		}
		if clientRunner != nil && backendRunner != nil {
			// Ensure all the packets are forwarded.
			msg := fmt.Sprintf("cmd:%d responseType:%d", ts.mc.cmd, ts.mb.respondType)
			require.Equal(t, ts.tc.backendIO.GetSequence(), ts.tc.clientIO.GetSequence(), msg)
		}
	} else {
		c(t, ts)
	}
}

// The client connects to the backend through the proxy.
func (ts *testSuite) authenticateFirstTime(t *testing.T, c checker) {
	ts.runAndCheck(t, c, ts.mc.authenticate, ts.mb.authenticate, ts.mp.authenticateFirstTime)
	if c == nil {
		// Check the data received by client equals to the data sent from the server and vice versa.
		require.Equal(t, ts.mb.authSucceed, ts.mc.authSucceed)
		require.Equal(t, ts.mc.username, ts.mb.username)
		require.Equal(t, ts.mc.dbName, ts.mb.db)
		require.Equal(t, ts.mc.authData, ts.mb.authData)
		require.Equal(t, ts.mc.attrs, ts.mb.attrs)
	}
}

// The proxy reconnects to the proxy using preserved client data.
// This must be called after authenticateFirstTime.
func (ts *testSuite) authenticateSecondTime(t *testing.T, c checker) {
	// The server won't request switching auth-plugin this time.
	ts.mb.backendConfig.switchAuth = false
	ts.mb.backendConfig.authSucceed = true
	ts.runAndCheck(t, c, nil, ts.mb.authenticate, ts.mp.authenticateSecondTime)
	if c == nil {
		require.Equal(t, ts.mc.username, ts.mb.username)
		require.Equal(t, ts.mc.dbName, ts.mb.db)
		require.Equal(t, []byte(ts.mp.sessionToken), ts.mb.authData)
	}
}

// Test forwarding commands between the client and the server.
// It verifies that it won't hang or report errors, and all the packets are forwarded.
func (ts *testSuite) executeCmd(t *testing.T, c checker) {
	ts.runAndCheck(t, c, ts.mc.request, ts.mb.respond, ts.mp.processCmd)
}

// Execute multiple commands at once to reuse the same ComProcessor.
func (ts *testSuite) executeMultiCmd(t *testing.T, cfgs []cfgOverrider, c checker) {
	for _, cfg := range cfgs {
		ts.setConfig(cfg)
		ts.runAndCheck(t, nil, ts.mc.request, ts.mb.respond, ts.mp.processCmd)
	}
	// Only check it at last.
	if c != nil {
		c(t, ts)
	}
}

// Test querying from the backend directly.
// It verifies that it won't hang or panic, and column / row counts match.
func (ts *testSuite) query(t *testing.T, c checker) {
	ts.runAndCheck(t, c, nil, ts.mb.respond, ts.mp.directQuery)
	if c == nil {
		if ts.mb.respondType == responseTypeResultSet {
			require.Equal(t, ts.mb.columns, len(ts.mp.rs.Fields))
			require.Equal(t, ts.mb.rows, len(ts.mp.rs.RowDatas))
		}
	}
}