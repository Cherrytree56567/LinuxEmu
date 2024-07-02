cd src
GOOS=js GOARCH=wasm go build -o ../main.wasm
cd ../
gcc tests/test.c -masm=intel -static -nostdlib -fcf-protection=none -o test.elf