package main

import (
    "bytes"
    "debug/elf"
    "fmt"
    "sync"
    "syscall/js"
)

type process struct {
    startAddress uint64
    entryPoint   uint64
    bin          []byte
}

func readELF(bin []byte, entrySymbol string) (*process, error) {

    elffile, err := elf.NewFile(bytes.NewReader(bin))
    if err != nil {
        return nil, err
    }

    symbols, err := elffile.Symbols()
    if err != nil {
        return nil, err
    }

    var entryPoint uint64
    for _, sym := range symbols {
        if sym.Name == entrySymbol && elf.STT_FUNC == elf.ST_TYPE(sym.Info) && elf.STB_GLOBAL == elf.ST_BIND(sym.Info) {
            entryPoint = sym.Value
        }
    }

    if entryPoint == 0 {
        return nil, fmt.Errorf("Could not find entrypoint symbol: %s", entrySymbol)
    }

    var startAddress uint64
    for _, sec := range elffile.Sections {
        if sec.Type != elf.SHT_NULL {
            startAddress = sec.Addr - sec.Offset
            break
        }
    }

    if startAddress == 0 {
        return nil, fmt.Errorf("Could not determine start address")
    }

    return &process{
        startAddress: startAddress,
        entryPoint:   entryPoint,
        bin:          bin,
    }, nil
}

func GetInput() ([]byte) {
	document := js.Global().Get("document")
	fileInput := document.Call("getElementById", "fileInput")
	var result []byte
	var wg sync.WaitGroup

	wg.Add(1) // Increment the WaitGroup counter

	fileInput.Set("oninput", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		files := fileInput.Get("files")
		if files.Length() == 0 {
			wg.Done() // Decrement the WaitGroup counter if no files are selected
			return nil
		}
		file := files.Call("item", 0)
		promise := file.Call("arrayBuffer")
		promise.Call("then", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
			data := js.Global().Get("Uint8Array").New(args[0])
			dst := make([]byte, data.Get("length").Int())
			js.CopyBytesToGo(dst, data)

			out := string(dst)
			if len(out) > 100 {
				out = out[:100] + "..."
			}

			result = dst
			wg.Done() // Decrement the WaitGroup counter when the result is ready
			return nil
		}))

		return nil
	}))

	wg.Wait() // Block until the WaitGroup counter is zero
	return result
}

func main() {

    proc, err := readELF(GetInput(), "main")
    if err != nil {
        panic(err)
    }

    fmt.Printf("Start: 0x%x\nEntry: 0x%x\n", proc.startAddress, proc.entryPoint)
}