module openflux-volga-lab

go 1.26.8

require (
	github.com/hashicorp/yamux v0.1.2
	universal-bypass-tool v0.0.0
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/pierrec/lz4/v4 v4.1.27 // indirect
)

replace universal-bypass-tool => ../
