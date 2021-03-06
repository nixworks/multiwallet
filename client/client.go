package client

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/OpenBazaar/golang-socketio"
	"github.com/OpenBazaar/golang-socketio/protocol"
	"github.com/OpenBazaar/multiwallet/client/transport"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcutil"
	"github.com/op/go-logging"
	"golang.org/x/net/proxy"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"
)

var log = logging.MustGetLogger("client")

type InsightClient struct {
	httpClient      http.Client
	apiUrl          url.URL
	blockNotifyChan chan Block
	txNotifyChan    chan Transaction
	socketClient    SocketClient
}

func NewInsightClient(apiUrl string, proxyDialer proxy.Dialer) (*InsightClient, error) {
	u, err := url.Parse(apiUrl)
	if err != nil {
		return nil, err
	}
	var port int
	var secure bool
	if u.Scheme == "https" {
		port = 443
		secure = true
	} else if u.Scheme == "http" {
		port = 80
		secure = false
	} else {
		return nil, errors.New("Unknown url scheme")
	}
	dial := net.Dial
	if proxyDialer != nil {
		dial = proxyDialer.Dial
	}
	socketClient, err := gosocketio.Dial(
		gosocketio.GetUrl(u.Host, port, secure),
		transport.GetDefaultWebsocketTransport(proxyDialer),
	)
	if err != nil {
		return nil, err
	}
	socketReady := make(chan struct{})
	socketClient.On(gosocketio.OnConnection, func(h *gosocketio.Channel, args interface{}) {
		close(socketReady)
	})
	ticker := time.NewTicker(time.Second * 10)
	select {
	case <-ticker.C:
		return nil, errors.New("Timed out waiting for websocket connection")
	case <-socketReady:
		break
	}

	bch := make(chan Block)
	tch := make(chan Transaction)
	tbTransport := &http.Transport{Dial: dial}
	ic := &InsightClient{
		http.Client{Timeout: time.Second * 30, Transport: tbTransport},
		*u,
		bch,
		tch,
		socketClient,
	}
	ic.setupListeners()
	return ic, nil
}

func (i *InsightClient) Close() {
	i.socketClient.Close()
}

func (i *InsightClient) doRequest(endpoint, method string, body io.Reader, query url.Values) (*http.Response, error) {
	requestUrl := i.apiUrl
	requestUrl.Path = path.Join(i.apiUrl.Path, endpoint)
	req, err := http.NewRequest(method, requestUrl.String(), body)
	if query != nil {
		req.URL.RawQuery = query.Encode()
	}
	if err != nil {
		return nil, fmt.Errorf("creating request: %s\n", err)
	}
	req.Header.Add("Content-Type", "application/json")

	resp, err := i.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	// Try again if for some reason it returned a bad request
	if resp.StatusCode == http.StatusBadRequest {
		resp, err = i.httpClient.Do(req)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status not ok: %s\n", resp.Status)
	}
	return resp, nil
}

func (i *InsightClient) GetTransaction(txid string) (*Transaction, error) {
	resp, err := i.doRequest("tx/"+txid, http.MethodGet, nil, nil)
	if err != nil {
		return nil, err
	}
	tx := new(Transaction)
	decoder := json.NewDecoder(resp.Body)
	defer resp.Body.Close()
	if err = decoder.Decode(tx); err != nil {
		return nil, fmt.Errorf("error decoding transactions: %s\n", err)
	}
	for n, in := range tx.Inputs {
		f, err := toFloat(in.ValueIface)
		if err != nil {
			return nil, err
		}
		tx.Inputs[n].Value = f
	}
	for n, out := range tx.Outputs {
		f, err := toFloat(out.ValueIface)
		if err != nil {
			return nil, err
		}
		tx.Outputs[n].Value = f
	}
	return tx, nil
}

func (i *InsightClient) GetTransactions(addrs []btcutil.Address) ([]Transaction, error) {
	var txs []Transaction
	from := 0
	for {
		tl, err := i.getTransactions(addrs, from, from+50)
		if err != nil {
			return txs, err
		}
		txs = append(txs, tl.Items...)
		if len(txs) >= tl.TotalItems {
			break
		}
		from += 50
	}
	return txs, nil
}

func (i *InsightClient) getTransactions(addrs []btcutil.Address, from, to int) (*TransactionList, error) {
	type req struct {
		Addrs string `json:"addrs"`
		From  int    `json:"from"`
		To    int    `json:"to"`
	}
	s := ``
	for n, addr := range addrs {
		s += addr.String()
		if n < len(addrs)-1 {
			s += ","
		}
	}
	r := &req{
		Addrs: s,
		From:  from,
		To:    to,
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	resp, err := i.doRequest("addrs/txs", http.MethodPost, bytes.NewReader(b), nil)
	if err != nil {
		return nil, err
	}
	tl := new(TransactionList)
	decoder := json.NewDecoder(resp.Body)
	defer resp.Body.Close()
	if err = decoder.Decode(tl); err != nil {
		return nil, fmt.Errorf("error decoding transaction list: %s\n", err)
	}
	for z, tx := range tl.Items {
		for n, in := range tx.Inputs {
			f, err := toFloat(in.ValueIface)
			if err != nil {
				return nil, err
			}
			tl.Items[z].Inputs[n].Value = f
		}
		for n, out := range tx.Outputs {
			f, err := toFloat(out.ValueIface)
			if err != nil {
				return nil, err
			}
			tl.Items[z].Outputs[n].Value = f
		}
	}
	return tl, nil
}

func (i *InsightClient) GetUtxos(addrs []btcutil.Address) ([]Utxo, error) {
	type req struct {
		Addrs string `json:"addrs"`
	}
	s := ``
	for n, addr := range addrs {
		s += addr.String()
		if n < len(addrs)-1 {
			s += ","
		}
	}
	r := &req{
		Addrs: s,
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	resp, err := i.doRequest("addrs/utxo", http.MethodPost, bytes.NewReader(b), nil)
	if err != nil {
		return nil, err
	}
	utxos := []Utxo{}
	decoder := json.NewDecoder(resp.Body)
	defer resp.Body.Close()
	if err = decoder.Decode(&utxos); err != nil {
		return nil, fmt.Errorf("error decoding utxo list: %s\n", err)
	}
	for z, u := range utxos {
		f, err := toFloat(u.AmountIface)
		if err != nil {
			return nil, err
		}
		utxos[z].Amount = f
	}
	return utxos, nil
}

func (i *InsightClient) BlockNotify() <-chan Block {
	return i.blockNotifyChan
}

func (i *InsightClient) TransactionNotify() <-chan Transaction {
	return i.txNotifyChan
}

func (i *InsightClient) ListenAddress(addr btcutil.Address) {
	var args []interface{}
	args = append(args, "bitcoind/addresstxid")
	args = append(args, []string{addr.String()})
	i.socketClient.Emit("subscribe", args)
}

func (i *InsightClient) setupListeners() {
	i.socketClient.On("bitcoind/hashblock", func(h *gosocketio.Channel, arg interface{}) {
		best, err := i.GetBestBlock()
		if err != nil {
			log.Errorf("Error downloading best block: %s", err.Error())
			return
		}
		i.blockNotifyChan <- *best
	})
	i.socketClient.Emit("subscribe", protocol.ToArgArray("bitcoind/hashblock"))

	i.socketClient.On("bitcoind/addresstxid", func(h *gosocketio.Channel, arg interface{}) {
		m, ok := arg.(map[string]interface{})
		if !ok {
			log.Errorf("Error checking type after socket notification: %T", arg)
			return
		}
		for _, v := range m {
			txid, ok := v.(string)
			if !ok {
				fmt.Println(arg)
				log.Errorf("Error checking type after socket notification: %T", arg)
				return
			}
			_, err := chainhash.NewHashFromStr(txid) // Check is 256 bit hash. Might also be address
			if err == nil {
				tx, err := i.GetTransaction(txid)
				if err != nil {
					log.Errorf("Error downloading tx after socket notification: %s", err.Error())
					return
				}
				i.txNotifyChan <- *tx
			}
		}
	})
}

func (i *InsightClient) Broadcast(tx []byte) (string, error) {
	txHex := hex.EncodeToString(tx)
	type RawTx struct {
		Raw string `json:"rawtx"`
	}
	t := RawTx{txHex}
	txJson, err := json.Marshal(&t)
	if err != nil {
		return "", err
	}
	resp, err := i.doRequest("tx/send", http.MethodPost, bytes.NewBuffer(txJson), nil)
	decoder := json.NewDecoder(resp.Body)

	type txid struct {
		Txid string `json:"txid"`
	}
	defer resp.Body.Close()
	id := new(txid)
	if err = decoder.Decode(id); err != nil {
		return "", fmt.Errorf("error decoding txid: %s\n", err)
	}
	return id.Txid, nil
}

func (i *InsightClient) GetBestBlock() (*Block, error) {
	q, err := url.ParseQuery("limit=2")
	if err != nil {
		return nil, err
	}
	resp, err := i.doRequest("blocks", http.MethodGet, nil, q)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(resp.Body)
	sl := new(BlockSummaryList)
	defer resp.Body.Close()
	if err = decoder.Decode(sl); err != nil {
		return nil, fmt.Errorf("error decoding block list: %s\n", err)
	}
	if len(sl.Blocks) != 2 {
		return nil, fmt.Errorf("API returned incorrect number of block summaries")
	}
	sum := sl.Blocks[0]
	sum.Parent = sl.Blocks[1].Hash
	return &sum, nil
}

// API sometimees returns a float64 or a string so we'll always convert it into a float64
func toFloat(i interface{}) (float64, error) {
	_, fok := i.(float64)
	_, sok := i.(string)
	if fok {
		return i.(float64), nil
	} else if sok {
		s := i.(string)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("error parsing value float: %s\n", err)
		}
		return f, nil
	} else {
		return 0, errors.New("Unknown value type in response")
	}
}
