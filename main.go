package main

import (
	"bytes"
	"debug/elf"
	"fmt"
	"os"
	"strconv"
	"strings"
    "sync"
    "syscall/js"
)

type process struct {
    startAddress uint64
    entryPoint   uint64
    bin          []byte
}

type cpu struct {
    proc    *process
    mem     []byte
    regfile *registerFile
    tick    chan bool
}

type register int

const (
    // These are in order of encoding value (i.e. rbp is 5)
    rax register = iota
    rcx
    rdx
    rbx
    rsp
    rbp
    rsi
    rdi
    r8
    r9
    r10
    r11
    r12
    r13
    r14
    r15
    rip
    rflags
)

var registerMap = map[register]string{
    rax:    "rax",
    rcx:    "rcx",
    rdx:    "rdx",
    rbx:    "rbx",
    rsp:    "rsp",
    rbp:    "rbp",
    rsi:    "rsi",
    rdi:    "rdi",
    r8:     "r8",
    r9:     "r9",
    r10:    "r10",
    r11:    "r11",
    r12:    "r12",
    r13:    "r13",
    r14:    "r14",
    r15:    "r15",
    rip:    "rip",
    rflags: "rflags",
}

type registerFile [18]uint64

func (regfile *registerFile) get(r register) uint64 {
    return regfile[r]
}

func (regfile *registerFile) set(r register, v uint64) {
    regfile[r] = v
}

func writeBytes(to []byte, start uint64, bytes int, val uint64) {
    for i := 0; i < bytes; i++ {
        to[start+uint64(i)] = byte(val >> (8 * i) & 0xFF)
    }
}

func readBytes(from []byte, start uint64, bytes int) uint64 {
    val := uint64(0)
    for i := 0; i < bytes; i++ {
        val |= uint64(from[start+uint64(i)]) << (8 * i)
    }

    return val
}

func hbdebug(msg string, bs []byte) {
    str := "%s:"
    args := []interface{}{msg}
    for _, b := range bs {
        str = str + " %x"
        args = append(args, b)
    }
    fmt.Printf(str+"\n", args...)
}

var prefixBytes = []byte{0x48}

func (c *cpu) loop(entryReturnAddress uint64) {
    for {
        <-c.tick

        ip := c.regfile.get(rip)
        if ip == entryReturnAddress {
            break
        }

        inb1 := c.mem[ip]

        widthPrefix := 32
        for {
            isPrefixByte := false
            for _, prefixByte := range prefixBytes {
                if prefixByte == inb1 {
                    isPrefixByte = true
                    break
                }
            }

            if !isPrefixByte {
                break
            }

            // 64 bit prefix signifier
            if inb1 == 0x48 {
                widthPrefix = 64
            } else {
                hbdebug("prog", c.mem[ip:ip+10])
                panic("Unknown prefix instruction")
            }

            ip++
            inb1 = c.mem[ip]
        }

        if inb1 >= 0x50 && inb1 < 0x58 { // push
            regvalue := c.regfile.get(register(inb1 - 0x50))
            sp := c.regfile.get(rsp)
            writeBytes(c.mem, sp-8, 8, regvalue)
            c.regfile.set(rsp, uint64(sp-8))
        }  else if inb1 == 0x89 { // mov r/m16/32/64, r/m16/32/64
            ip++
            inb2 := c.mem[ip]
            rhs := register((inb2 & 0b00111000) >> 3)
            lhs := register(inb2 & 0b111)
            c.regfile.set(lhs, c.regfile.get(rhs))
        } else if inb1 >= 0xB8 && inb1 < 0xC0 { // mov r16/32/64, imm16/32/64
            lreg := register(inb1 - 0xB8)
            val := readBytes(c.mem, ip+uint64(1), widthPrefix/8)
            ip += uint64(widthPrefix / 8)
            c.regfile.set(lreg, val)
        } else if inb1 >= 0x58 && inb1 < 0x60 { // pop
            lhs := register(inb1 - 0x58)
            sp := c.regfile.get(rsp)
            c.regfile.set(lhs, readBytes(c.mem, sp, 8))
            c.regfile.set(rsp, uint64(sp+8))
        } else if inb1 == 0xC3 { // ret
            sp := c.regfile.get(rsp)
            retAddress := readBytes(c.mem, sp, 8)
            c.regfile.set(rsp, uint64(sp+8))
            c.regfile.set(rip, retAddress)
            continue
        } else {
            hbdebug("prog", c.mem[ip:ip+10])
            panic("Unknown instruction")
        }

        c.regfile.set(rip, ip+1)
    }
}

func (c *cpu) run(proc *process) {
    copy(c.mem[proc.startAddress:proc.startAddress+uint64(len(proc.bin))], proc.bin)
    c.regfile.set(rip, proc.entryPoint)
    initialStackPointer := uint64(len(c.mem)-8)
    writeBytes(c.mem, initialStackPointer, 8, initialStackPointer)
    c.regfile.set(rsp, initialStackPointer)
    c.loop(initialStackPointer)
    os.Exit(int(c.regfile.get(rax)))
}

func newCPU(memory uint64) cpu {
    return cpu{
        mem:     make([]byte, memory),
        regfile: &registerFile{},
        tick:    make(chan bool, 1),
    }
}

func (c *cpu) resolveDebuggerValue(dval string) (uint64, error) {
    for reg, val := range registerMap {
        if val == dval {
            return c.regfile.get(reg), nil
        }
    }

    if len(dval) > 2 && (dval[:2] == "0x" || dval[:2] == "0X") {
        return strconv.ParseUint(dval[2:], 16, 64)
    }

    return strconv.ParseUint(dval, 10, 64)
}

func repl(c *cpu) {
    fmt.Println("go-amd64-emulator REPL")
    help := `commands:
    s/step:             continue to next instruction
    r/registers [$reg]:     print all register values or just $reg
    d/decimal:          toggle hex/decimal printing
    m/memory $from $count:      print memory values starting at $from until $from+$count
    h/help:             print this`
    fmt.Println(help)

    intFormat := "%d"

    // Set up an event listener for input changes
    inputChan := make(chan string, 1)
    inputs := js.Global().Get("document").Call("getElementById", "DebugInput")
    inputs.Call("addEventListener", "input", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
        input := inputs.Get("value").String()
        inputChan <- input // Send input value to channel
        return nil
    }))

    for {
        fmt.Printf("> ")

        // Wait for input asynchronously
        input := <-inputChan

        if input == "ccccc" {
            continue
        } else {
            inputs.Set("value", "") // Clear input field
            parts := strings.Split(input, " ")

            switch parts[0] {
            case "h":
                fallthrough
            case "help":
                fmt.Println(help)

            case "m":
                fallthrough
            case "memory":
                msg := "Invalid arguments: m/memory $from $to; use hex (0x10), decimal (10), or register name (rsp)"
                if len(parts) != 3 {
                    fmt.Println(msg)
                    continue
                }

                from, err := c.resolveDebuggerValue(parts[1])
                if err != nil {
                    fmt.Println(msg)
                    continue
                }

                to, err := c.resolveDebuggerValue(parts[2])
                if err != nil {
                    fmt.Println(msg)
                    continue
                }

                hbdebug(fmt.Sprintf("memory["+intFormat+":"+intFormat+"]", from, from+to), c.mem[from:from+to])

            case "d":
                fallthrough
            case "decimal":
                if intFormat == "%d" {
                    intFormat = "0x%x"
                    fmt.Println("Numbers displayed as hex")
                } else {
                    intFormat = "%d"
                    fmt.Println("Numbers displayed as decimal")
                }

            case "r":
                fallthrough
            case "registers":
                filter := ""
                if len(parts) > 1 {
                    filter = parts[1]
                }

                for i := 0; i < len(registerMap); i++ {
                    reg := register(i)
                    name := registerMap[reg]
                    if filter != "" && filter != name {
                        continue
                    }

                    fmt.Printf("%s:\t"+intFormat+"\n", name, c.regfile.get(reg))
                }

            case "s":
                fallthrough
            case "step":
                c.tick <- true
            }
        }
    }
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

    debug := js.Global().Call("DebuggingEnabled").Bool()


    // 10 MB
    cpu := newCPU(0x400000 * 10)

    go cpu.run(proc)
    if debug {
        repl(&cpu)
    } else {
        for {
            cpu.tick <- true
        }
    }
}