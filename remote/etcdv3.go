package remote



import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/gottingen/lung"
	"github.com/gottingen/kgb/log"
	"go.etcd.io/etcd/clientv3"
)

type EtcdV3 struct {
	client *clientv3.Client
}

func (p *EtcdV3) Get(rp lung.RemoteProvider) (io.Reader, error) {
	client, err := p.getClient(rp.Endpoint())
	if err != nil {
		return nil, err
	}

	resp, err := client.Get(context.Background(), rp.Path())
	if err != nil {
		return nil, err
	}

	if len(resp.Kvs) == 0 {
		return nil, errors.New("kv length equal 0")
	}

	return bytes.NewReader(resp.Kvs[0].Value), nil
}

func (p *EtcdV3) Watch(rp lung.RemoteProvider) (io.Reader, error) {
	return p.Get(rp)
}

func (p *EtcdV3) WatchChannel(rp lung.RemoteProvider) (<-chan *lung.RemoteResponse, chan bool) {
	rr := make(chan *lung.RemoteResponse)

	client, err := p.getClient(rp.Endpoint())
	if err != nil {
		log.Logger.Warn("get client failed: %v", err)
		rr <- &lung.RemoteResponse{Error: err}
		close(rr)
		return rr, nil
	}

	done := make(chan bool)
	watcher := client.Watch(context.Background(), rp.Path())
	go func(done chan bool) {
		for {
			select {
			case <-done:
				return
			case wr := <-watcher:
				if wr.Canceled {
					return
				}

				for _, ev := range wr.Events {
					rr <- &lung.RemoteResponse{Value: ev.Kv.Value, Error: err}
				}
			}
		}
	}(done)

	return rr, done
}

func (p *EtcdV3) getClient(endpoint string) (clnt *clientv3.Client, err error) {
	if p.client != nil {
		return p.client, nil
	}

	clnt, err = clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(endpoint, ","),
		DialTimeout: time.Second * 30,
	})

	if err != nil {
		return nil, err
	}

	p.client = clnt
	return
}
