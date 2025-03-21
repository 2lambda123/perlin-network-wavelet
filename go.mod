module github.com/perlin-network/wavelet

go 1.12

replace github.com/go-interpreter/wagon => github.com/perlin-network/wagon v0.3.1-0.20180825141017-f8cb99b55a39

replace github.com/dgraph-io/badger/v2 => github.com/perlin-network/badger/v2 v2.0.1

require (
	github.com/armon/go-radix v1.0.0
	github.com/benpye/readline v0.0.0-20181117181432-5ff4ccac79cf
	github.com/buaazp/fasthttprouter v0.1.1
	github.com/dgraph-io/badger/v2 v2.0.0
	github.com/djherbis/buffer v1.1.0
	github.com/fasthttp/websocket v1.4.0
	github.com/gogo/protobuf v1.3.2
	github.com/golang/protobuf v1.5.3
	github.com/golang/snappy v0.0.4
	github.com/gorilla/websocket v1.4.1
	github.com/huandu/skiplist v0.0.0-20180112095830-8e883b265e1b
	github.com/minio/highwayhash v1.0.0
	github.com/perlin-network/life v0.0.0-20190723115110-3091ed0c1be8
	github.com/perlin-network/noise v1.1.1-0.20191113101947-c8dc081eafa7
	github.com/phayes/freeport v0.0.0-20180830031419-95f893ade6f2
	github.com/phf/go-queue v0.0.0-20170504031614-9abe38d0371d
	github.com/pkg/errors v0.9.1
	github.com/rcrowley/go-metrics v0.0.0-20181016184325-3113b8401b8a
	github.com/rs/zerolog v1.14.3
	github.com/stretchr/testify v1.8.3
	github.com/syndtr/goleveldb v1.0.0
	github.com/valyala/bytebufferpool v1.0.0
	github.com/valyala/fasthttp v1.34.0
	github.com/valyala/fastjson v1.4.1
	go.uber.org/atomic v1.5.0
	golang.org/x/crypto v0.35.0
	golang.org/x/net v0.36.0
	golang.org/x/time v0.3.0
	google.golang.org/grpc v1.56.3
	gopkg.in/urfave/cli.v1 v1.20.0
)
