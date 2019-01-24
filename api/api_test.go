package api_test

import (
	"fmt"
	"github.com/perlin-network/graph/wire"
	"github.com/perlin-network/noise/crypto"
	"github.com/perlin-network/wavelet"
	"github.com/perlin-network/wavelet/api"
	apiClient "github.com/perlin-network/wavelet/cmd/wctl/client"
	"github.com/perlin-network/wavelet/node"
	"github.com/perlin-network/wavelet/params"
	"github.com/perlin-network/wavelet/security"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"io/ioutil"
	"net"
	"net/http"
	"testing"
	"time"
)

const (
	host           = "localhost"
	privateKeyFile = "../cmd/wavelet/wallet.txt"
)

func setupMockServer(port int, privateKeyFile string, mockWavelet node.NodeInterface) (*http.Server, *apiClient.Client, error) {
	privateKeyBytes, err := ioutil.ReadFile(privateKeyFile)
	if err != nil {
		return nil, nil, err
	}
	keys, err := crypto.FromPrivateKey(security.SignaturePolicy, string(privateKeyBytes))
	if err != nil {
		return nil, nil, err
	}
	sc := make(chan *http.Server)
	go api.Run(nil, mockWavelet, sc, api.Options{
		ListenAddr: fmt.Sprintf("%s:%d", "localhost", port),
		Clients: []*api.ClientInfo{
			&api.ClientInfo{
				PublicKey: keys.PublicKeyHex(),
				Permissions: api.ClientPermissions{
					CanSendTransaction: true,
					CanPollTransaction: true,
					CanControlStats:    true,
					CanAccessLedger:    true,
				},
			},
		},
	})
	server := <-sc
	time.Sleep(50 * time.Millisecond)

	client, err := client(port, privateKeyFile)
	if err != nil {
		return nil, nil, err
	}
	return server, client, nil
}

func client(port int, privateKeyFile string) (*apiClient.Client, error) {
	privateKeyBytes, err := ioutil.ReadFile(privateKeyFile)
	if err != nil {
		return nil, errors.Wrapf(err, "Unable to open api private key file: %s", privateKeyFile)
	}

	client, err := apiClient.NewClient(apiClient.Config{
		APIHost:    host,
		APIPort:    uint(port),
		PrivateKey: string(privateKeyBytes),
		UseHTTPS:   false,
	})
	if err != nil {
		return nil, err
	}

	err = client.Init()
	if err != nil {
		return nil, err
	}

	return client, nil
}

// GetRandomUnusedPort returns a random unused port
func GetRandomUnusedPort() int {
	listener, _ := net.Listen("tcp", ":0")
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

////////////////////////////////

func Test_api_serverVersion(t *testing.T) {
	port := GetRandomUnusedPort()
	s, c, err := setupMockServer(port, privateKeyFile, &node.WaveletMock{})
	assert.Nil(t, err)
	defer s.Close()

	res, err := c.ServerVersion()
	assert.Nil(t, err)
	assert.Equal(t, params.Version, res.Version)
}

func Test_api_send_transaction(t *testing.T) {
	port := GetRandomUnusedPort()
	s, c, err := setupMockServer(port, privateKeyFile, &node.WaveletMock{
		MakeTransactionCB: func(tag uint32, payload []byte) *wire.Transaction {
			return &wire.Transaction{}
		},
	})
	assert.Nil(t, err)
	defer s.Close()

	payload := fmt.Sprintf(`{
		"recipient": "%s",
		"body": {
			"Payload": "Register"
		}
	}`, "contractAddress")
	res, err := c.SendTransaction(params.TagCreateContract, []byte(payload))
	assert.Nil(t, err)
	assert.True(t, len(res.TransactionID) > 0)
}

func Test_api_ledger_state(t *testing.T) {
	port := GetRandomUnusedPort()
	s, c, err := setupMockServer(port, privateKeyFile, &node.WaveletMock{
		LedgerDoCB: func(f func(ledger wavelet.LedgerInterface)) {
			mock := &wavelet.MockLedger{}
			mock.SnapshotCB = func() map[string]interface{} {
				return map[string]interface{}{
					"key": "value",
				}
			}
			f(mock)
		},
	})
	assert.Nil(t, err)
	defer s.Close()

	res, err := c.LedgerState()
	assert.Nil(t, err)
	assert.Equal(t, "value", res.State["key"])
}
