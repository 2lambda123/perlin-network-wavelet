package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/perlin-network/wavelet"
	"github.com/phayes/freeport"
	"github.com/stretchr/testify/assert"
)

var wallet1 = "87a6813c3b4cf534b6ae82db9b1409fa7dbd5c13dba5858970b56084c4a930eb400056ee68a7cc2695222df05ea76875bc27ec6e61e8e62317c336157019c405"
var wallet2 = "85e7450f7cf0d9cd1d1d7bf4169c2f364eea4ba833a7280e0f931a1d92fd92c2696937c2c8df35dba0169de72990b80761e51dd9e2411fa1fce147f68ade830a"
var wallet3 = "5b9fcd2d6f8e34f4aa472e0c3099fefd25f0ceab9e908196b1dda63e55349d22f03bb6f98c4dfd31f3d448c7ec79fa3eaa92250112ada43471812f4b1ace6467"

func TestMain(t *testing.T) {
	w := NewTestWavelet(t, nil)
	defer w.Cleanup()

	ledger := w.GetLedgerStatus(t)
	assert.EqualValues(t, "127.0.0.1:"+w.Port, ledger.Address)
	assert.NotEqual(t, wallet1[64:], ledger.PublicKey) // A random wallet should be generated
}

func TestMain_WithWalletString(t *testing.T) {
	wallet := "b27b880e6e44e3b127186a08bc5698316e8dd99157cec56211560b62141f0851c72096021609681eb8cab244752945b2008e1b51d8bc2208b2b562f35485d5cc"
	w := NewTestWavelet(t, &TestWaveletConfig{wallet})
	defer w.Cleanup()

	ledger := w.GetLedgerStatus(t)
	assert.EqualValues(t, wallet[64:], ledger.PublicKey)
}

func TestMain_WithWalletFile(t *testing.T) {
	wallet := "d6acf5caca96e9da2088bc3a051ed938749145c3f3f7b5bd81aefeb46c12f9e901e4d5b4f51ef76a9a9cc511af910964943404172347b6a01bcfe65d768c9354"

	// Write wallet to a temporary file
	dir, err := ioutil.TempDir("", "wavelet")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	walletPath := filepath.Join(dir, "wallet.txt")
	if err := ioutil.WriteFile(walletPath, []byte(wallet), 0666); err != nil {
		t.Fatal(err)
	}

	w := NewTestWavelet(t, &TestWaveletConfig{walletPath})
	defer w.Cleanup()

	ledger := w.GetLedgerStatus(t)
	assert.EqualValues(t, wallet[64:], ledger.PublicKey)
}

func TestMain_WithInvalidWallet(t *testing.T) {
	wallet := "foobar"
	w := NewTestWavelet(t, &TestWaveletConfig{wallet})
	defer w.Cleanup()

	ledger := w.GetLedgerStatus(t)
	assert.NotEqual(t, wallet1[64:], ledger.PublicKey) // A random wallet should be generated
}

func TestMain_Status(t *testing.T) {
	w := NewTestWavelet(t, nil)
	defer w.Cleanup()

	w.Stdin <- "status"
	w.Stdout.Search(t, "Here is the current status of your node")
}

func TestMain_Pay(t *testing.T) {
	alice := NewTestWavelet(t, &TestWaveletConfig{wallet2})
	defer alice.Cleanup()

	bob := alice.Testnet.AddNode(t, 0)

	time.Sleep(time.Second * 1)

	recipient := bob.PublicKey()
	alice.Stdin <- fmt.Sprintf("p %s 99999", hex.EncodeToString(recipient[:]))

	txID := extractTxID(t, alice.Stdout.Search(t, "Success! Your payment transaction ID:"))
	tx := alice.WaitForTransction(t, txID)

	assert.EqualValues(t, txID, tx.ID)
	assert.EqualValues(t, alice.PublicKey, tx.Sender)
	assert.EqualValues(t, alice.PublicKey, tx.Creator)

	<-bob.WaitForConsensus()
	assert.EqualValues(t, 99999, bob.Balance())
}

func TestMain_Spawn(t *testing.T) {
	w := NewTestWavelet(t, &TestWaveletConfig{wallet2})
	defer w.Cleanup()

	for i := 0; i < 3; i++ {
		w.Testnet.AddNode(t, 0)
	}

	time.Sleep(time.Second * 1)

	w.Stdin <- "spawn ../../testdata/transfer_back.wasm"

	txID := extractTxID(t, w.Stdout.Search(t, "Success! Your smart contracts ID:"))
	tx := w.WaitForTransction(t, txID)

	assert.EqualValues(t, txID, tx.ID)
	assert.EqualValues(t, w.PublicKey, tx.Sender)
	assert.EqualValues(t, w.PublicKey, tx.Creator)
}

func TestMain_Call(t *testing.T) {
	w := NewTestWavelet(t, &TestWaveletConfig{wallet2})
	defer w.Cleanup()

	for i := 0; i < 3; i++ {
		w.Testnet.AddNode(t, 0)
	}

	time.Sleep(time.Second * 1)

	w.Stdin <- "spawn ../../testdata/transfer_back.wasm"

	txID := extractTxID(t, w.Stdout.Search(t, "Success! Your smart contracts ID:"))

	w.WaitForConsensus(t)

	tx := w.WaitForTransction(t, txID)
	w.Stdin <- fmt.Sprintf("call %s 1000 100000 on_money_received", tx.ID)
	w.Stdout.Search(t, "Your smart contract invocation transaction ID:")
}

func TestMain_DepositGas(t *testing.T) {
	w := NewTestWavelet(t, &TestWaveletConfig{wallet2})
	defer w.Cleanup()

	for i := 0; i < 3; i++ {
		w.Testnet.AddNode(t, 0)
	}

	time.Sleep(time.Second * 1)

	w.Stdin <- "spawn ../../testdata/transfer_back.wasm"

	txID := extractTxID(t, w.Stdout.Search(t, "Success! Your smart contracts ID:"))

	w.WaitForConsensus(t)

	tx := w.WaitForTransction(t, txID)
	w.Stdin <- fmt.Sprintf("deposit-gas %s 99999", tx.ID)
	w.Stdout.Search(t, "Your gas deposit transaction ID:")
}

func TestMain_Find(t *testing.T) {
	alice := NewTestWavelet(t, &TestWaveletConfig{wallet2})
	defer alice.Cleanup()

	bob := alice.Testnet.AddNode(t, 0)

	time.Sleep(time.Second * 1)

	recipient := bob.PublicKey()
	alice.Stdin <- fmt.Sprintf("p %s 99999", hex.EncodeToString(recipient[:]))

	txID := extractTxID(t, alice.Stdout.Search(t, "Success! Your payment transaction ID:"))
	alice.WaitForTransction(t, txID)

	alice.Stdin <- fmt.Sprintf("find %s", txID)
	alice.Stdout.Search(t, fmt.Sprintf("Transaction: %s", txID))
}

func TestMain_PlaceStake(t *testing.T) {
	alice := NewTestWavelet(t, &TestWaveletConfig{wallet2})
	defer alice.Cleanup()

	bob := alice.Testnet.AddNode(t, 0)

	time.Sleep(time.Second * 1)

	alice.Stdin <- "ps 1000"

	txID := extractTxID(t, alice.Stdout.Search(t, "Success! Your stake placement transaction ID:"))
	tx := alice.WaitForTransction(t, txID)

	assert.EqualValues(t, txID, tx.ID)
	assert.EqualValues(t, alice.PublicKey, tx.Sender)
	assert.EqualValues(t, alice.PublicKey, tx.Creator)

	<-bob.WaitForConsensus()
	assert.EqualValues(t, 1000, bob.StakeWithPublicKey(asAccountID(t, alice.PublicKey)))
}

func TestMain_WithdrawStake(t *testing.T) {
	alice := NewTestWavelet(t, &TestWaveletConfig{wallet2})
	defer alice.Cleanup()

	bob := alice.Testnet.AddNode(t, 0)

	time.Sleep(time.Second * 1)

	alice.Stdin <- "ps 1000"

	txID := extractTxID(t, alice.Stdout.Search(t, "Success! Your stake placement transaction ID:"))
	alice.WaitForTransction(t, txID)

	alice.WaitForConsensus(t)

	alice.Stdin <- "ws 500"

	txID = extractTxID(t, alice.Stdout.Search(t, "Success! Your stake withdrawal transaction ID:"))
	tx := alice.WaitForTransction(t, txID)

	assert.EqualValues(t, txID, tx.ID)
	assert.EqualValues(t, alice.PublicKey, tx.Sender)
	assert.EqualValues(t, alice.PublicKey, tx.Creator)

	<-bob.WaitForConsensus()
	assert.EqualValues(t, 500, bob.StakeWithPublicKey(asAccountID(t, alice.PublicKey)))
}

func TestMain_WithdrawReward(t *testing.T) {
	w := NewTestWavelet(t, &TestWaveletConfig{wallet2})
	defer w.Cleanup()

	for i := 0; i < 3; i++ {
		w.Testnet.AddNode(t, 0)
	}

	time.Sleep(time.Second * 1)

	w.Stdin <- "wr 1000"

	txID := extractTxID(t, w.Stdout.Search(t, "Success! Your reward withdrawal transaction ID:"))
	tx := w.WaitForTransction(t, txID)

	assert.EqualValues(t, txID, tx.ID)
	assert.EqualValues(t, w.PublicKey, tx.Sender)
	assert.EqualValues(t, w.PublicKey, tx.Creator)

	// TODO: check if reward is actually withdrawn
}

func nextPort(t *testing.T) string {
	port, err := freeport.GetFreePort()
	if err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(port)
}

func waitForAPI(t *testing.T, apiPort string) {
	timeout := time.NewTimer(time.Second * 5)
	tick := time.NewTicker(time.Second * 1)

	for {
		select {
		case <-timeout.C:
			t.Fatal("timed out waiting for API")

		case <-tick.C:
			if _, err := getLedgerStatus(apiPort); err == nil {
				return
			}
		}
	}
}

type TestLedgerStatus struct {
	PublicKey string     `json:"public_key"`
	Address   string     `json:"address"`
	Peers     []TestPeer `json:"peers"`
}

type TestPeer struct {
	Address   string `json:"address"`
	PublicKey string `json:"public_key"`
}

type TestTransaction struct {
	ID      string `json:"id"`
	Sender  string `json:"sender"`
	Creator string `json:"creator"`
}

func nopStdin() io.ReadCloser {
	return ioutil.NopCloser(strings.NewReader(""))
}

type mockStdin chan string

func (s mockStdin) Read(dst []byte) (n int, err error) {
	line := <-s
	if line == "" {
		return 0, io.EOF
	}

	copy(dst, []byte(line+"\n"))
	return len(line) + 1, nil
}

func (s mockStdin) Close() error {
	close(s)
	return nil
}

type mockStdout struct {
	Lines chan string
	buf   []byte
	bi    int
}

func newMockStdout() *mockStdout {
	return &mockStdout{
		Lines: make(chan string, 256),
		buf:   make([]byte, 0),
	}
}

func (s *mockStdout) Write(p []byte) (n int, err error) {
	s.buf = append(s.buf, p...)

	ni := bytes.Index(p, []byte{'\n'})
	if ni < 0 {
		return len(p), nil
	}

	buf := make([]byte, len(p))
	copy(buf, p)
	for ni >= 0 {
		str := string(s.buf[:ni])
		s.Lines <- str

		s.buf = s.buf[ni+1:]
		buf = buf[ni+1:]
		ni = bytes.Index(buf, []byte{'\n'})
	}

	return len(p), nil
}

func (s *mockStdout) Search(t *testing.T, sub string) string {
	t.Helper()

	timeout := time.NewTimer(time.Second * 5)
	for {
		select {
		case line := <-s.Lines:
			if strings.Contains(line, sub) {
				return line
			}

		case <-timeout.C:
			t.Fatal("timed out searching for string in stdout")
		}
	}
}

func extractTxID(t *testing.T, s string) string {
	if len(s) < 64 {
		t.Fatal("output does not contain tx id")
	}

	txID := s[len(s)-64:]
	if _, err := hex.DecodeString(txID); err != nil {
		t.Fatal(err)
	}

	return txID
}

func waitFor(t *testing.T, fn func() error) {
	timeout := time.NewTimer(time.Second * 10)
	ticker := time.NewTicker(time.Second * 1)

	for {
		select {
		case <-timeout.C:
			t.Fatal("timed out waiting")

		case <-ticker.C:
			if err := fn(); err == nil {
				return
			}
		}
	}
}

type TestWavelet struct {
	Testnet   *wavelet.TestNetwork
	Port      string
	APIPort   string
	PublicKey string
	Stdin     mockStdin
	Stdout    *mockStdout
}

func (w *TestWavelet) Cleanup() {
	close(w.Stdin)
	w.Testnet.Cleanup()

	// Since Ledger doesn't have a proper cleanup method yet, just sleep
	time.Sleep(time.Millisecond * 500)
}

type TestWaveletConfig struct {
	Wallet string
}

func NewTestWavelet(t *testing.T, cfg *TestWaveletConfig) *TestWavelet {
	testnet := wavelet.NewTestNetwork(t)

	port := nextPort(t)
	apiPort := nextPort(t)

	args := []string{"wavelet", "--port", port, "--api.port", apiPort}
	if cfg != nil && cfg.Wallet != "" {
		args = append(args, []string{"--wallet", cfg.Wallet}...)
	}

	// Bootstrap with the faucet
	args = append(args, testnet.Faucet().Addr())

	stdin := mockStdin(make(chan string))
	stdout := newMockStdout()

	go Run(args, stdin, stdout, true)
	waitForAPI(t, apiPort)

	w := &TestWavelet{
		Testnet: testnet,
		Port:    port,
		APIPort: apiPort,
		Stdin:   stdin,
		Stdout:  stdout,
	}
	w.PublicKey = w.GetLedgerStatus(t).PublicKey

	return w
}

func (w *TestWavelet) GetLedgerStatus(t *testing.T) *TestLedgerStatus {
	ledger, err := getLedgerStatus(w.APIPort)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func getLedgerStatus(apiPort string) (*TestLedgerStatus, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://localhost:%s/ledger", apiPort), nil)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*500)
	defer cancel()

	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("expecting GET /ledger to return 200, got %d instead", resp.StatusCode)
	}

	var ledger TestLedgerStatus
	if err := json.NewDecoder(resp.Body).Decode(&ledger); err != nil {
		return nil, err
	}

	return &ledger, nil
}

func (w *TestWavelet) WaitForTransction(t *testing.T, id string) *TestTransaction {
	var tx *TestTransaction
	var err error
	waitFor(t, func() error {
		tx, err = getTransaction(w.APIPort, id)
		return err
	})

	return tx
}

func (w *TestWavelet) GetTransaction(t *testing.T, id string) *TestTransaction {
	tx, err := getTransaction(w.APIPort, id)
	if err != nil {
		t.Fatal(err)
	}

	return tx
}

func getTransaction(apiPort string, id string) (*TestTransaction, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://localhost:%s/tx/%s", apiPort, id), nil)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*500)
	defer cancel()

	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("expecting GET /tx/%s to return 200, got %d instead", id, resp.StatusCode)
	}

	var tx TestTransaction
	if err := json.NewDecoder(resp.Body).Decode(&tx); err != nil {
		return nil, err
	}

	return &tx, nil
}

func (w *TestWavelet) WaitForConsensus(t *testing.T) {
	t.Helper()
	w.Stdout.Search(t, "Finalized consensus round")
}

func asAccountID(t *testing.T, s string) wavelet.AccountID {
	t.Helper()

	var accountID wavelet.AccountID
	key, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}

	copy(accountID[:], key)
	return accountID
}
