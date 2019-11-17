mkdir "/workspace"
export GOROOT=/usr/local/go
export GOPATH=/workspace/
export PATH=$PATH:$GOROOT/bin
export PATH=$PATH:$GOPATH/bin
export GO111MODULE=on
mkdir -p "/workspace/src/example"
mkdir "/workspace/pkg"
cd /workspace/src/example || exit
curl -O https://storage.googleapis.com/golang/go1.13.linux-amd64.tar.gz