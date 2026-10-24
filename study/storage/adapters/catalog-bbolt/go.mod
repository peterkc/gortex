module gxstudy-catalog-bbolt

go 1.27.1

require (
	go.etcd.io/bbolt v1.5.0
	gxstudy v0.0.0
)

require golang.org/x/sys v0.45.0 // indirect

replace gxstudy => ../../harness
