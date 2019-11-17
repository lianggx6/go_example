mkdir "/workspace"
export GOROOT=/usr/local/go
export GOPATH=/workspace/
export PATH=$PATH:$GOROOT/bin
export PATH=$PATH:$GOPATH/bin
export GO111MODULE=on
mkdir -p "/workspace/src/example"
mkdir "/workspace/pkg"
cd /workspace/src/example || exit
curl -O https://github.com/lianggx6/go_example/blob/master/main.go
curl -O https://github.com/lianggx6/go_example/blob/master/go.mod
go run main.go