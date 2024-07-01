GOOS=js GOARCH=wasm go build -o main.wasm
gcc tests/test.c -static -nostdlib