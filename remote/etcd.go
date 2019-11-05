package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/gottingen/lung"
	etcd "go.etcd.io/etcd/client"
)

type Decoder interface {
	Decode(io.Reader) (interface{}, error)
}

type Etcdv2 struct {
	Decoder
	lung.RemoteProvider

	Username string
	Password string
}

func (c *Etcdv2) Get(rp lung.RemoteProvider) (io.Reader, error) {
	c.verify(rp)
	c.RemoteProvider = rp

	return c.get()
}

func (c *Etcdv2) Watch(rp lung.RemoteProvider) (io.Reader, error) {
	c.verify(rp)
	c.RemoteProvider = rp

	return c.get()
}

func (c *Etcdv2) WatchChannel(rp lung.RemoteProvider) (<-chan *lung.RemoteResponse, chan bool) {
	c.verify(rp)
	c.RemoteProvider = rp

	watcher, err := c.watcher()
	if err != nil {
		return nil, nil
	}

	rr := make(chan *lung.RemoteResponse)
	done := make(chan bool)

	ctx, cancel := context.WithCancel(context.Background())

	go func(done <-chan bool) {
		select {
		case <-done:
			cancel()
		}
	}(done)

	go func(ctx context.Context, rr chan<- *lung.RemoteResponse) {
		for {
			res, err := watcher.Next(ctx)

			if err == context.Canceled {
				return
			}

			if err != nil {
				rr <- &lung.RemoteResponse{
					Error: err,
				}

				continue
			}

			rr <- &lung.RemoteResponse{
				Value: c.readr(res.Node),
			}
		}

	}(ctx, rr)

	return rr, done
}

func (c Etcdv2) verify(rp lung.RemoteProvider) {
	if rp.Provider() != "etcd" {
		panic("lung-etcd remote supports only etcd.")
	}

	if rp.SecretKeyring() != "" {
		panic("lung-etcd doesn't support keyrings, use Decoder instead.")
	}
}

func (c Etcdv2) newEtcdClient() (etcd.KeysAPI, error) {
	client, err := etcd.New(etcd.Config{
		Username: c.Username,
		Password: c.Password,

		Endpoints: []string{c.Endpoint()},
	})
	if err != nil {
		return nil, err
	}

	return etcd.NewKeysAPI(client), nil
}

func (c Etcdv2) get() (io.Reader, error) {
	kapi, err := c.newEtcdClient()
	if err != nil {
		return nil, err
	}

	res, err := kapi.Get(context.Background(), c.Path(), &etcd.GetOptions{
		Recursive: true,
	})
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(c.readr(res.Node)), nil
}

func (c Etcdv2) watcher() (etcd.Watcher, error) {
	kapi, err := c.newEtcdClient()
	if err != nil {
		return nil, err
	}

	return kapi.Watcher(c.Path(), &etcd.WatcherOptions{
		Recursive: true,
	}), nil
}

func (c Etcdv2) readr(node *etcd.Node) []byte {
	m, _ := json.Marshal(c.nodeVals(node, c.Path()).(map[string]interface{}))

	return m
}

func (c Etcdv2) nodeVals(node *etcd.Node, path string) interface{} {
	if !node.Dir && path == node.Key {

		if c.Decoder != nil {
			val, _ := c.Decode(strings.NewReader(node.Value))

			return val
		}

		return node.Value
	}

	vv := make(map[string]interface{})

	if len(node.Nodes) == 0 {
		newKey := keyFirstChild(strDiff(node.Key, path))
		vv[newKey] = c.nodeVals(node, pathSum(path, newKey))

		return vv
	}

	for _, n := range node.Nodes {
		vv[keyLastChild(n.Key)] = c.nodeVals(n, n.Key)
	}

	return vv
}

func keyLastChild(key string) string {
	kk := strings.Split(key, "/")

	return kk[len(kk)-1]
}

func keyFirstChild(key string) string {
	kk := strings.Split(key, "/")

	return kk[1]
}

func strDiff(s1, s2 string) string {
	return strings.ReplaceAll(s1, s2, "")
}

func pathSum(pp ...string) string {
	return strings.Join(pp, "/")
}
