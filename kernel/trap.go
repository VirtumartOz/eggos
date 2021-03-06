package kernel

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/icexin/eggos/debug"
	"github.com/icexin/eggos/kernel/trap"
	"github.com/icexin/eggos/pic"
	"github.com/icexin/eggos/sys"
)

//go:generate go run genvector.go

const (
	STS_IG32 = 0x8e
	DPL_KERN = 0x0

	IRQ_BASE = 0x20
)

type gateDesc struct {
	offsetLow  uint16
	selector   uint16
	dcount     uint8
	attr       uint8
	offsetHigh uint16
}

var (
	idt    [256]gateDesc
	idtptr [6]byte

	traptask threadptr
)

var (
	// 因为中断处理是异步的，在获取一次中断期间可能发生了多次中断，
	// irqset按位保存发生的中断，对应的中断号为IRQ_BASE+1<<bit
	irqset uintptr
)

//go:nosplit
func idt_init()

//go:nosplit
func trapret()

//go:nosplit
func setGateDesc(gate *gateDesc, handler func(), tp, pl uint8) {
	base := uint32(sys.FuncPC(handler))
	gate.offsetLow = uint16(base & 0xffff)
	gate.selector = 0x08
	gate.dcount = 0
	gate.attr = tp | pl<<5
	gate.offsetHigh = uint16((base >> 16) & 0xffff)
}

//go:nosplit
func fillidt() {
	if unsafe.Sizeof(gateDesc{}) != 8 {
		panic("invalid gateDesc size")
	}

	for i := 0; i < 256; i++ {
		setGateDesc(&idt[i], vectors[i], STS_IG32, DPL_KERN)
	}

	limit := (*uint16)(unsafe.Pointer(&idtptr[0]))
	base := (*uint32)(unsafe.Pointer(&idtptr[2]))
	*limit = uint16(unsafe.Sizeof(idt) - 1)
	*base = uint32(uintptr(unsafe.Pointer(&idt[0])))
}

//go:nosplit
func ignoreHandler() {
}

//go:nosplit
func pageFaultHandler() {
	Signal(uintptr(syscall.SIGSEGV), 2, sys.Cr2())
}

//go:nosplit
func faultHandler() {
	my := Mythread()
	Signal(uintptr(syscall.SIGABRT), my.tf.Trapno, 0)
}

//go:nosplit
func trappanic() {
	panic("trap panic")
}

//go:nosplit
func PreparePanic(tf *TrapFrame) {
	ChangeReturnPC(tf, sys.FuncPC(trappanic))
}

// ChangeReturnPC change the return pc of a trap
// must be called in trap handler
//go:nosplit
func ChangeReturnPC(tf *TrapFrame, pc uintptr) {
	tf.Err, tf.IP, tf.CS, tf.FLAGS = pc, tf.CS, tf.FLAGS, tf.IP
	tf.SP -= sys.PtrSize
}

//go:nosplit
func dotrap(tf *TrapFrame) {
	handler := trap.Handler(int(tf.Trapno))
	if handler == nil {
		faultHandler()
		return
	}
	// timer and syscall interrupts are processed synchronously
	if tf.Trapno > 32 && tf.Trapno != 0x80 {
		// pci using level trigger irq, cause dead lock on trap handler
		// FIXME: hard code network irq line
		if tf.Trapno == 43 {
			pic.DisableIRQ(43 - pic.IRQ_BASE)
		}
		wakeIRQ(tf.Trapno)
		return
	}
	handler()
}

func traploop() {
	runtime.LockOSThread()
	var trapset uintptr
	const setsize = unsafe.Sizeof(irqset) * 8

	my := Mythread()
	traptask = (threadptr)(unsafe.Pointer(my))
	debug.Logf("[trap] tid:%d", my.id)
	for {
		trapset, _, _ = syscall.Syscall(SYS_WAIT_IRQ, 0, 0, 0)
		for i := uintptr(0); i < setsize; i++ {
			if trapset&(1<<i) == 0 {
				continue
			}
			trapno := uintptr(IRQ_BASE + i)

			handler := trap.Handler(int(trapno))
			if handler == nil {
				fmt.Printf("trap handler for %d not found\n", trapno)
				pic.EOI(trapno)
				continue
			}
			handler()
		}
	}
}

//go:nosplit
func trap_init() {
	idt_init()
	trap.Register(14, pageFaultHandler)
	trap.Register(39, ignoreHandler)
	trap.Register(47, ignoreHandler)
}

//go:nosplit
func wakeIRQ(no uintptr) {
	irqset |= 1 << (no - IRQ_BASE)
	wakeup(&irqset, 1)
	Yield()
}

//go:nosplit
func waitIRQ() uintptr {
	if irqset != 0 {
		ret := irqset
		irqset = 0
		return ret
	}
	sleepon(&irqset)
	ret := irqset
	irqset = 0
	return ret
}
