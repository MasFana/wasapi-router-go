//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
)

const (
	AUDCLNT_STREAMFLAGS_LOWLATENCY = 0x08000000
	THREAD_PRIORITY_TIME_CRITICAL  = 15
)

var (
	modavrt                          = windows.NewLazySystemDLL("avrt.dll")
	procAvSetMmThreadCharacteristics = modavrt.NewProc("AvSetMmThreadCharacteristicsW")
	procAvRevertMmThreadChars        = modavrt.NewProc("AvRevertMmThreadCharacteristics")

	modkernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procSetThreadPriority = modkernel32.NewProc("SetThreadPriority")
)

// ============================================================================
// 1. LOCK-FREE SPSC RING BUFFER WITH FRACTIONAL DRIFT RESAMPLER & DE-ZIPPERING
// ============================================================================

type SpscRingBuffer struct {
	buf      []float32
	capacity uint64
	writePos uint64 // atomic
	readPos  uint64 // atomic
	underrun uint64 // atomic
	overrun  uint64 // atomic

	// Single-consumer state (owned exclusively by renderTargetThread)
	currentGain float32
	resamplePos float64
}

func NewSpscRingBuffer(frameCapacity uint64) *SpscRingBuffer {
	return &SpscRingBuffer{
		buf:         make([]float32, frameCapacity*2),
		capacity:    frameCapacity,
		currentGain: 1.0,
	}
}

func (r *SpscRingBuffer) AvailableRead() uint64 {
	w := atomic.LoadUint64(&r.writePos)
	rd := atomic.LoadUint64(&r.readPos)
	if w < rd {
		return 0
	}
	return w - rd
}

func (r *SpscRingBuffer) AvailableWrite() uint64 {
	used := r.AvailableRead()
	if used >= r.capacity {
		return 0
	}
	return r.capacity - used
}

func (r *SpscRingBuffer) Reset() {
	atomic.StoreUint64(&r.writePos, 0)
	atomic.StoreUint64(&r.readPos, 0)
	r.resamplePos = 0
}

func (r *SpscRingBuffer) WriteStereo(src []float32, frames uint64) uint64 {
	avail := r.AvailableWrite()
	if frames > avail {
		atomic.AddUint64(&r.overrun, 1)
		drop := frames - avail
		atomic.AddUint64(&r.readPos, drop)
	}
	if frames == 0 {
		return 0
	}

	w := atomic.LoadUint64(&r.writePos)
	cap := r.capacity
	for i := uint64(0); i < frames; i++ {
		idx := ((w + i) % cap) * 2
		srcIdx := i * 2
		r.buf[idx] = src[srcIdx]
		r.buf[idx+1] = src[srcIdx+1]
	}

	atomic.StoreUint64(&r.writePos, w+frames)
	return frames
}

// ReadStereoAdaptive performs click-free Fractional Resampling to keep latency locked
// around targetLatencyFrames (~10ms) and applies per-sample exponential volume smoothing.
func (r *SpscRingBuffer) ReadStereoAdaptive(dst []float32, outFrames uint64, targetDspGain float32, muted bool, targetLatencyFrames uint64) uint64 {
	avail := r.AvailableRead()
	if avail < 4 || outFrames == 0 {
		if outFrames > 0 {
			atomic.AddUint64(&r.underrun, 1)
			for i := uint64(0); i < outFrames*2; i++ {
				dst[i] = 0
			}
		}
		return 0
	}

	goalGain := targetDspGain
	if muted {
		goalGain = 0.0
	}

	// Proportional drift controller: adjusts playback pitch by up to ±0.5% to lock buffer to target
	driftError := float64(int64(avail) - int64(targetLatencyFrames))
	stepRatio := 1.0 + clampFloat64(driftError*0.000008, -0.005, 0.005)

	maxNeeded := uint64(math.Ceil(float64(outFrames)*stepRatio)) + 2
	if maxNeeded > avail {
		stepRatio = float64(avail-2) / float64(outFrames)
		if stepRatio < 0.95 {
			atomic.AddUint64(&r.underrun, 1)
			for i := uint64(0); i < outFrames*2; i++ {
				dst[i] = 0
			}
			return 0
		}
	}

	rd := atomic.LoadUint64(&r.readPos)
	cap := r.capacity
	curGain := r.currentGain
	const smoothAlpha = 0.0025 // ~8ms time constant @ 48kHz (eliminates zipper noise)

	pos := r.resamplePos
	for i := uint64(0); i < outFrames; i++ {
		curGain += smoothAlpha * (goalGain - curGain)

		intOffset := uint64(pos)
		frac := float32(pos - float64(intOffset))

		idx0 := ((rd + intOffset) % cap) * 2
		idx1 := ((rd + intOffset + 1) % cap) * 2

		sL := r.buf[idx0] + frac*(r.buf[idx1]-r.buf[idx0])
		sR := r.buf[idx0+1] + frac*(r.buf[idx1+1]-r.buf[idx0+1])

		dstIdx := i * 2
		dst[dstIdx] = softLimit(sL * curGain)
		dst[dstIdx+1] = softLimit(sR * curGain)

		pos += stepRatio
	}

	consumed := uint64(pos)
	r.resamplePos = pos - float64(consumed)
	r.currentGain = curGain

	atomic.StoreUint64(&r.readPos, rd+consumed)
	return outFrames
}

// splitHybridVolume separates a [0.0 .. 1.5] UI slider into:
// 1. OS Native Hardware Scalar [0.0 .. 1.0] (controlled via IAudioEndpointVolume)
// 2. DSP Digital Boost Multiplier [1.0 .. 2.0] (only engages above 100% to avoid double-attenuation!)
func splitHybridVolume(slider float32) (osScalar float32, dspBoost float32) {
	if slider <= 0.001 {
		return 0.0, 0.0
	}
	if slider <= 1.0 {
		// 0% to 100%: Windows Native Volume handles 100% of the attenuation; DSP stays bit-perfect 1.0x!
		return slider, 1.0
	}
	// 100% to 150%: Lock Windows Native Volume at 100% (1.0) and apply up to +6dB (2.0x) digital boost
	return 1.0, 1.0 + (slider-1.0)*2.0
}

// effectiveTotalGain computes the combined perceptual gain for dB display in the UI
func effectiveTotalGain(slider float32) float32 {
	if slider <= 0.001 {
		return 0.0
	}
	if slider <= 1.0 {
		return slider
	}
	return 1.0 + (slider-1.0)*2.0
}

func softLimit(x float32) float32 {
	if x > -0.90 && x < 0.90 {
		return x
	}
	if x > 3.0 {
		return 1.0
	}
	if x < -3.0 {
		return -1.0
	}
	x2 := x * x
	return x * (27.0 + x2) / (27.0 + 9.0*x2)
}

func clampFloat64(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ============================================================================
// 2. SAFE PCM DECODER / ENCODER (Supports 16-bit, 24-bit, and 32-bit Float)
// ============================================================================

func decodeToStereoFloat(rawPtr *byte, frames int, channels int, blockAlign int, bitsPerSample int, dspGain float32, dstStereo []float32) {
	if rawPtr == nil || frames <= 0 || blockAlign <= 0 {
		for i := 0; i < frames*2; i++ {
			dstStereo[i] = 0
		}
		return
	}

	totalBytes := frames * blockAlign
	rawBytes := unsafe.Slice(rawPtr, totalBytes)
	bytesPerSample := blockAlign / channels
	if bytesPerSample <= 0 {
		bytesPerSample = 4
	}

	for f := 0; f < frames; f++ {
		frameOffset := f * blockAlign
		var l, r float32

		switch {
		case bitsPerSample == 32 && bytesPerSample == 4:
			lBits := binary.LittleEndian.Uint32(rawBytes[frameOffset : frameOffset+4])
			l = math.Float32frombits(lBits)
			if channels > 1 && frameOffset+8 <= totalBytes {
				rBits := binary.LittleEndian.Uint32(rawBytes[frameOffset+4 : frameOffset+8])
				r = math.Float32frombits(rBits)
			} else {
				r = l
			}

		case bitsPerSample == 24 && bytesPerSample == 3:
			lInt := int32(rawBytes[frameOffset]) | int32(rawBytes[frameOffset+1])<<8 | int32(int8(rawBytes[frameOffset+2]))<<16
			l = float32(lInt) / 8388608.0
			if channels > 1 && frameOffset+6 <= totalBytes {
				rInt := int32(rawBytes[frameOffset+3]) | int32(rawBytes[frameOffset+4])<<8 | int32(int8(rawBytes[frameOffset+5]))<<16
				r = float32(rInt) / 8388608.0
			} else {
				r = l
			}

		case bitsPerSample == 16 && bytesPerSample == 2:
			lInt := int16(binary.LittleEndian.Uint16(rawBytes[frameOffset : frameOffset+2]))
			l = float32(lInt) / 32768.0
			if channels > 1 && frameOffset+4 <= totalBytes {
				rInt := int16(binary.LittleEndian.Uint16(rawBytes[frameOffset+2 : frameOffset+4]))
				r = float32(rInt) / 32768.0
			} else {
				r = l
			}
		}

		if math.IsNaN(float64(l)) || math.IsInf(float64(l), 0) {
			l = 0
		}
		if math.IsNaN(float64(r)) || math.IsInf(float64(r), 0) {
			r = 0
		}

		dstStereo[f*2] = softLimit(l * dspGain)
		dstStereo[f*2+1] = softLimit(r * dspGain)
	}
}

func encodeFromStereoFloat(srcStereo []float32, rawPtr *byte, frames int, channels int, blockAlign int, bitsPerSample int) {
	if rawPtr == nil || frames <= 0 || blockAlign <= 0 {
		return
	}

	totalBytes := frames * blockAlign
	outBytes := unsafe.Slice(rawPtr, totalBytes)
	clear(outBytes)

	bytesPerSample := blockAlign / channels
	if bytesPerSample <= 0 {
		return
	}

	for f := 0; f < frames; f++ {
		l := srcStereo[f*2]
		r := srcStereo[f*2+1]

		frameOffset := f * blockAlign

		switch {
		case bitsPerSample == 32 && bytesPerSample == 4:
			binary.LittleEndian.PutUint32(outBytes[frameOffset:frameOffset+4], math.Float32bits(l))
			if channels > 1 && frameOffset+8 <= totalBytes {
				binary.LittleEndian.PutUint32(outBytes[frameOffset+4:frameOffset+8], math.Float32bits(r))
			}
			for c := 2; c < channels; c++ {
				off := frameOffset + c*4
				if off+4 <= totalBytes {
					mid := (l + r) * 0.35
					binary.LittleEndian.PutUint32(outBytes[off:off+4], math.Float32bits(mid))
				}
			}

		case bitsPerSample == 24 && bytesPerSample == 3:
			lInt := int32(l * 8388607.0)
			outBytes[frameOffset] = byte(lInt)
			outBytes[frameOffset+1] = byte(lInt >> 8)
			outBytes[frameOffset+2] = byte(lInt >> 16)
			if channels > 1 && frameOffset+6 <= totalBytes {
				rInt := int32(r * 8388607.0)
				outBytes[frameOffset+3] = byte(rInt)
				outBytes[frameOffset+4] = byte(rInt >> 8)
				outBytes[frameOffset+5] = byte(rInt >> 16)
			}

		case bitsPerSample == 16 && bytesPerSample == 2:
			lInt := int16(l * 32767.0)
			binary.LittleEndian.PutUint16(outBytes[frameOffset:frameOffset+2], uint16(lInt))
			if channels > 1 && frameOffset+4 <= totalBytes {
				rInt := int16(r * 32767.0)
				binary.LittleEndian.PutUint16(outBytes[frameOffset+2:frameOffset+4], uint16(rInt))
			}
		}
	}
}

// ============================================================================
// 3. LOW-LATENCY WASAPI ENGINE + NATIVE WINDOWS VOLUME SYNC (IAudioEndpointVolume)
// ============================================================================

func boostProAudioThread() func() {
	var taskIndex uint32
	taskName, err := syscall.UTF16PtrFromString("Pro Audio")
	if err != nil {
		return func() {}
	}

	r0, _, _ := procAvSetMmThreadCharacteristics.Call(
		uintptr(unsafe.Pointer(taskName)),
		uintptr(unsafe.Pointer(&taskIndex)),
	)
	if r0 == 0 {
		return func() {}
	}

	curThread := windows.CurrentThread()
	_, _, _ = procSetThreadPriority.Call(
		uintptr(curThread),
		uintptr(THREAD_PRIORITY_TIME_CRITICAL),
	)

	return func() {
		_, _, _ = procAvRevertMmThreadChars.Call(r0)
	}
}

type DeviceInfo struct {
	ID        string
	Name      string
	IsDefault bool
}

type TargetState struct {
	Device      DeviceInfo
	ActiveFlag  uint32 // atomic (0 or 1)
	MutedFlag   uint32 // atomic (0 or 1)
	VolumeBits  uint32 // atomic float32 slider [0.0 .. 1.5]
	LastUserSet int64  // atomic unix nano timestamp (prevents OS poll bounce during rapid key presses)
	Ring        *SpscRingBuffer
	StopChan    chan struct{}

	SampleRate uint32 // atomic
	Channels   uint32 // atomic
	Bits       uint32 // atomic
	BufferUs   uint32 // atomic (microseconds)
}

func (t *TargetState) IsActive() bool { return atomic.LoadUint32(&t.ActiveFlag) == 1 }
func (t *TargetState) IsMuted() bool  { return atomic.LoadUint32(&t.MutedFlag) == 1 }
func (t *TargetState) GetVolume() float32 {
	return math.Float32frombits(atomic.LoadUint32(&t.VolumeBits))
}
func (t *TargetState) SetVolume(v float32) {
	if v < 0.0 {
		v = 0.0
	} else if v > 1.5 {
		v = 1.5
	}
	atomic.StoreUint32(&t.VolumeBits, math.Float32bits(v))
}

type endpointVolCmd struct {
	deviceID  string
	setVol    bool
	volScalar float32
	setMute   bool
	mute      bool
}

type AudioTelemetry struct {
	PeakL       float64
	PeakR       float64
	PeakHoldL   float64
	PeakHoldR   float64
	RMSL        float64
	RMSR        float64
	Correlation float64
	Balance     float64
	Spectrum    [16]float64
	SpecHold    [16]float64
	SampleRate  uint32
	Channels    uint32
	Bits        uint32
	PeriodMs    float64
	MMCSSActive bool
}

type AudioEngine struct {
	mu            sync.RWMutex
	Devices       []DeviceInfo
	SourceIndex   int
	Targets       map[string]*TargetState
	CaptureStop   chan struct{}
	IsCapturing   bool
	MasterMute    uint32
	MasterVolBits uint32
	TargetFloorMs uint32 // atomic target latency floor in ms (default 10ms)

	volCmdChan chan endpointVolCmd

	telemetryMu sync.Mutex
	Telemetry   AudioTelemetry
}

func NewAudioEngine() *AudioEngine {
	eng := &AudioEngine{
		Targets:       make(map[string]*TargetState),
		SourceIndex:   0,
		TargetFloorMs: 10,
		volCmdChan:    make(chan endpointVolCmd, 64),
	}
	eng.SetMasterVolumeInternal(1.0)
	go eng.osVolumeSyncWorker()
	return eng
}

func (e *AudioEngine) SetMasterVolumeInternal(v float32) {
	if v < 0 {
		v = 0
	} else if v > 1.5 {
		v = 1.5
	}
	atomic.StoreUint32(&e.MasterVolBits, math.Float32bits(v))
}

// SetMasterVolume updates the source endpoint's Windows Native Volume + DSP Boost
func (e *AudioEngine) SetMasterVolume(v float32) {
	if v < 0 {
		v = 0
	} else if v > 1.5 {
		v = 1.5
	}
	e.SetMasterVolumeInternal(v)

	e.mu.RLock()
	var srcID string
	if e.SourceIndex >= 0 && e.SourceIndex < len(e.Devices) {
		srcID = e.Devices[e.SourceIndex].ID
	}
	var srcTarget *TargetState
	if srcID != "" {
		srcTarget = e.Targets[srcID]
	}
	e.mu.RUnlock()

	if srcTarget != nil {
		srcTarget.SetVolume(v)
		atomic.StoreInt64(&srcTarget.LastUserSet, time.Now().UnixNano())
	}
	if srcID != "" {
		osScalar, _ := splitHybridVolume(v)
		e.queueOSVolumeUpdate(srcID, true, osScalar, false, false)
	}
}

func (e *AudioEngine) GetMasterVolume() float32 {
	return math.Float32frombits(atomic.LoadUint32(&e.MasterVolBits))
}

// ToggleMasterMute toggles both the internal master mute and the source device's Windows Native Mute
func (e *AudioEngine) ToggleMasterMute() bool {
	var isMuted bool
	for {
		cur := atomic.LoadUint32(&e.MasterMute)
		next := cur ^ 1
		if atomic.CompareAndSwapUint32(&e.MasterMute, cur, next) {
			isMuted = (next == 1)
			break
		}
	}

	e.mu.RLock()
	var srcID string
	if e.SourceIndex >= 0 && e.SourceIndex < len(e.Devices) {
		srcID = e.Devices[e.SourceIndex].ID
	}
	var srcTarget *TargetState
	if srcID != "" {
		srcTarget = e.Targets[srcID]
	}
	e.mu.RUnlock()

	if srcTarget != nil {
		if isMuted {
			atomic.StoreUint32(&srcTarget.MutedFlag, 1)
		} else {
			atomic.StoreUint32(&srcTarget.MutedFlag, 0)
		}
		atomic.StoreInt64(&srcTarget.LastUserSet, time.Now().UnixNano())
	}
	if srcID != "" {
		e.queueOSVolumeUpdate(srcID, false, 0, true, isMuted)
	}
	return isMuted
}

func (e *AudioEngine) IsMasterMuted() bool {
	return atomic.LoadUint32(&e.MasterMute) == 1
}

// SetDeviceVolume adjusts a specific endpoint's volume AND immediately overrides Windows Native Volume
func (e *AudioEngine) SetDeviceVolume(deviceID string, newVol float32) {
	if newVol < 0.0 {
		newVol = 0.0
	} else if newVol > 1.5 {
		newVol = 1.5
	}

	e.mu.RLock()
	t := e.Targets[deviceID]
	isSource := e.SourceIndex >= 0 && e.SourceIndex < len(e.Devices) && e.Devices[e.SourceIndex].ID == deviceID
	e.mu.RUnlock()

	if t != nil {
		t.SetVolume(newVol)
		atomic.StoreInt64(&t.LastUserSet, time.Now().UnixNano())
	}
	if isSource {
		e.SetMasterVolumeInternal(newVol)
	}

	osScalar, _ := splitHybridVolume(newVol)
	e.queueOSVolumeUpdate(deviceID, true, osScalar, false, false)
}

// ToggleDeviceMute toggles a specific endpoint's mute state AND overrides Windows Native Mute
func (e *AudioEngine) ToggleDeviceMute(deviceID string) bool {
	e.mu.RLock()
	t := e.Targets[deviceID]
	isSource := e.SourceIndex >= 0 && e.SourceIndex < len(e.Devices) && e.Devices[e.SourceIndex].ID == deviceID
	e.mu.RUnlock()

	if t == nil {
		return false
	}

	var muted bool
	for {
		cur := atomic.LoadUint32(&t.MutedFlag)
		next := cur ^ 1
		if atomic.CompareAndSwapUint32(&t.MutedFlag, cur, next) {
			muted = (next == 1)
			break
		}
	}
	atomic.StoreInt64(&t.LastUserSet, time.Now().UnixNano())

	if isSource {
		if muted {
			atomic.StoreUint32(&e.MasterMute, 1)
		} else {
			atomic.StoreUint32(&e.MasterMute, 0)
		}
	}

	e.queueOSVolumeUpdate(deviceID, false, 0, true, muted)
	return muted
}

func (e *AudioEngine) queueOSVolumeUpdate(deviceID string, setVol bool, volScalar float32, setMute bool, mute bool) {
	select {
	case e.volCmdChan <- endpointVolCmd{
		deviceID:  deviceID,
		setVol:    setVol,
		volScalar: volScalar,
		setMute:   setMute,
		mute:      mute,
	}:
	default:
	}
}

// osVolumeSyncWorker runs a dedicated COM thread that:
// 1. Applies volume/mute changes directly to Windows native IAudioEndpointVolume
// 2. Polls Windows native volume every 150ms so hardware media keys / taskbar changes sync back into the TUI!
func (e *AudioEngine) osVolumeSyncWorker() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator,
		0,
		wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator,
		&mmde,
	); err != nil {
		return
	}
	defer mmde.Release()

	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case cmd := <-e.volCmdChan:
			applyEndpointVolumeChange(mmde, cmd)

		case <-ticker.C:
			e.pollAllEndpointsFromOS(mmde)
		}
	}
}

func applyEndpointVolumeChange(mmde *wca.IMMDeviceEnumerator, cmd endpointVolCmd) {
	dev := getDeviceByID(mmde, cmd.deviceID)
	if dev == nil {
		return
	}
	defer dev.Release()

	var aev *wca.IAudioEndpointVolume
	if err := dev.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err != nil || aev == nil {
		return
	}
	defer aev.Release()

	if cmd.setVol {
		scalar := cmd.volScalar
		if scalar < 0.0 {
			scalar = 0.0
		} else if scalar > 1.0 {
			scalar = 1.0
		}
		_ = aev.SetMasterVolumeLevelScalar(scalar, nil)
	}
	if cmd.setMute {
		_ = aev.SetMute(cmd.mute, nil)
	}
}

func (e *AudioEngine) pollAllEndpointsFromOS(mmde *wca.IMMDeviceEnumerator) {
	e.mu.RLock()
	devices := make([]DeviceInfo, len(e.Devices))
	copy(devices, e.Devices)
	srcIdx := e.SourceIndex
	e.mu.RUnlock()

	now := time.Now().UnixNano()

	for idx, d := range devices {
		e.mu.RLock()
		t := e.Targets[d.ID]
		e.mu.RUnlock()
		if t == nil {
			continue
		}

		// Grace period of 450ms after user presses volume keys in TUI so OS polling doesn't fight user input
		if now-atomic.LoadInt64(&t.LastUserSet) < int64(450*time.Millisecond) {
			continue
		}

		dev := getDeviceByID(mmde, d.ID)
		if dev == nil {
			continue
		}

		var aev *wca.IAudioEndpointVolume
		if err := dev.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err == nil && aev != nil {
			var osVol float32
			var osMute bool
			if err1 := aev.GetMasterVolumeLevelScalar(&osVol); err1 == nil {
				curSlider := t.GetVolume()
				// If slider is in >100% boost range and Windows is still near 100%, preserve the user's boost
				if !(curSlider > 1.0 && osVol >= 0.98) {
					t.SetVolume(osVol)
					if idx == srcIdx {
						e.SetMasterVolumeInternal(osVol)
					}
				}
			}
			if err2 := aev.GetMute(&osMute); err2 == nil {
				muteVal := uint32(0)
				if osMute {
					muteVal = 1
				}
				atomic.StoreUint32(&t.MutedFlag, muteVal)
				if idx == srcIdx {
					atomic.StoreUint32(&e.MasterMute, muteVal)
				}
			}
			aev.Release()
		}
		dev.Release()
	}
}

func (e *AudioEngine) RefreshDevices() error {
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
		defer ole.CoUninitialize()

		var mmde *wca.IMMDeviceEnumerator
		if err := wca.CoCreateInstance(
			wca.CLSID_MMDeviceEnumerator,
			0,
			wca.CLSCTX_ALL,
			wca.IID_IMMDeviceEnumerator,
			&mmde,
		); err != nil {
			errCh <- err
			return
		}
		defer mmde.Release()

		var defaultDev *wca.IMMDevice
		var defaultID string
		if err := mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &defaultDev); err == nil && defaultDev != nil {
			_ = defaultDev.GetId(&defaultID)
			defaultDev.Release()
		}

		var dc *wca.IMMDeviceCollection
		if err := mmde.EnumAudioEndpoints(wca.ERender, wca.DEVICE_STATE_ACTIVE, &dc); err != nil {
			errCh <- err
			return
		}
		defer dc.Release()

		var count uint32
		if err := dc.GetCount(&count); err != nil {
			errCh <- err
			return
		}

		type polledEndpoint struct {
			info   DeviceInfo
			osVol  float32
			osMute bool
		}

		var list []polledEndpoint
		for i := uint32(0); i < count; i++ {
			var dev *wca.IMMDevice
			if err := dc.Item(i, &dev); err != nil || dev == nil {
				continue
			}

			var id string
			_ = dev.GetId(&id)

			name := fmt.Sprintf("Endpoint #%d", i)
			var ps *wca.IPropertyStore
			if err := dev.OpenPropertyStore(wca.STGM_READ, &ps); err == nil && ps != nil {
				var pv wca.PROPVARIANT
				if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err == nil {
					if s := pv.String(); s != "" {
						name = s
					}
				}
				ps.Release()
			}

			// Read initial Windows Native Volume & Mute state for every endpoint
			osVol := float32(1.0)
			osMute := false
			var aev *wca.IAudioEndpointVolume
			if err := dev.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err == nil && aev != nil {
				_ = aev.GetMasterVolumeLevelScalar(&osVol)
				_ = aev.GetMute(&osMute)
				aev.Release()
			}
			dev.Release()

			list = append(list, polledEndpoint{
				info: DeviceInfo{
					ID:        id,
					Name:      name,
					IsDefault: id == defaultID,
				},
				osVol:  osVol,
				osMute: osMute,
			})
		}

		e.mu.Lock()
		devList := make([]DeviceInfo, len(list))
		for idx, item := range list {
			devList[idx] = item.info
			if item.info.IsDefault && !e.IsCapturing {
				e.SourceIndex = idx
			}
			ts, exists := e.Targets[item.info.ID]
			if !exists {
				ts = &TargetState{
					Device: item.info,
					Ring:   NewSpscRingBuffer(96000),
				}
				e.Targets[item.info.ID] = ts
			}
			ts.SetVolume(item.osVol)
			if item.osMute {
				atomic.StoreUint32(&ts.MutedFlag, 1)
			} else {
				atomic.StoreUint32(&ts.MutedFlag, 0)
			}
			if idx == e.SourceIndex {
				e.SetMasterVolumeInternal(item.osVol)
				if item.osMute {
					atomic.StoreUint32(&e.MasterMute, 1)
				} else {
					atomic.StoreUint32(&e.MasterMute, 0)
				}
			}
		}
		e.Devices = devList
		e.mu.Unlock()
		errCh <- nil
	}()
	return <-errCh
}

func (e *AudioEngine) RestartSourceCapture(sourceIdx int) {
	e.mu.Lock()
	if e.IsCapturing && e.CaptureStop != nil {
		close(e.CaptureStop)
	}
	if sourceIdx >= 0 && sourceIdx < len(e.Devices) {
		e.SourceIndex = sourceIdx
	}
	if len(e.Devices) == 0 {
		e.mu.Unlock()
		return
	}
	srcDev := e.Devices[e.SourceIndex]
	if t, ok := e.Targets[srcDev.ID]; ok {
		e.SetMasterVolumeInternal(t.GetVolume())
		if t.IsMuted() {
			atomic.StoreUint32(&e.MasterMute, 1)
		} else {
			atomic.StoreUint32(&e.MasterMute, 0)
		}
	}
	stopCh := make(chan struct{})
	e.CaptureStop = stopCh
	e.IsCapturing = true
	e.mu.Unlock()

	go e.captureLoopbackThread(srcDev.ID, stopCh)
}

func (e *AudioEngine) ToggleTarget(deviceID string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	t, ok := e.Targets[deviceID]
	if !ok {
		return
	}

	if t.IsActive() {
		if t.StopChan != nil {
			close(t.StopChan)
		}
		atomic.StoreUint32(&t.ActiveFlag, 0)
	} else {
		t.Ring.Reset()
		t.StopChan = make(chan struct{})
		atomic.StoreUint32(&t.ActiveFlag, 1)
		go e.renderTargetThread(t, t.StopChan)
	}
}

func (e *AudioEngine) captureLoopbackThread(deviceID string, stopCh chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	revertMMCSS := boostProAudioThread()
	defer revertMMCSS()

	_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &mmde); err != nil {
		return
	}
	defer mmde.Release()

	dev := getDeviceByID(mmde, deviceID)
	if dev == nil {
		return
	}
	defer dev.Release()

	var ac *wca.IAudioClient
	if err := dev.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
		return
	}
	defer ac.Release()

	var wfx *wca.WAVEFORMATEX
	if err := ac.GetMixFormat(&wfx); err != nil || wfx == nil {
		return
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(wfx)))

	channels := int(wfx.NChannels)
	sampleRate := uint32(wfx.NSamplesPerSec)
	blockAlign := int(wfx.NBlockAlign)
	bitsPerSample := int(wfx.WBitsPerSample)

	var defaultPeriod, minPeriod wca.REFERENCE_TIME
	_ = ac.GetDevicePeriod(&defaultPeriod, &minPeriod)
	if minPeriod == 0 {
		minPeriod = 30000 // 3ms fallback
	}
	if defaultPeriod == 0 {
		defaultPeriod = 100000
	}

	chosenPeriod := minPeriod
	baseFlags := uint32(wca.AUDCLNT_STREAMFLAGS_LOOPBACK | wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK)
	err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, baseFlags|AUDCLNT_STREAMFLAGS_LOWLATENCY, minPeriod, 0, wfx, nil)
	if err != nil {
		chosenPeriod = defaultPeriod
		if err = ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, baseFlags, defaultPeriod, 0, wfx, nil); err != nil {
			return
		}
	}

	periodMs := float64(chosenPeriod) / 10000.0

	hEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return
	}
	defer windows.CloseHandle(hEvent)

	if err := ac.SetEventHandle(uintptr(hEvent)); err != nil {
		return
	}

	var acc *wca.IAudioCaptureClient
	if err := ac.GetService(wca.IID_IAudioCaptureClient, &acc); err != nil {
		return
	}
	defer acc.Release()

	if err := ac.Start(); err != nil {
		return
	}
	defer ac.Stop()

	const maxScratchFrames = 16384
	stereoScratch := make([]float32, maxScratchFrames*2)

	for {
		select {
		case <-stopCh:
			return
		default:
		}

		waitRes, _ := windows.WaitForSingleObject(hEvent, 80)
		if waitRes != windows.WAIT_OBJECT_0 {
			e.decayTelemetry(sampleRate, uint32(channels), uint32(bitsPerSample), periodMs)
			continue
		}

		for {
			var packetLength uint32
			if err := acc.GetNextPacketSize(&packetLength); err != nil || packetLength == 0 {
				break
			}

			var data *byte
			var framesToRead uint32
			var dwFlags uint32
			var devPos, qpcPos uint64

			if err := acc.GetBuffer(&data, &framesToRead, &dwFlags, &devPos, &qpcPos); err != nil {
				break
			}

			if framesToRead > 0 {
				frames := int(framesToRead)
				if frames > maxScratchFrames {
					frames = maxScratchFrames
				}

				// Note: Windows Loopback taps BEFORE the endpoint hardware volume, OR after depending on driver.
				// Using splitHybridVolume ensures 0..100% controls the physical Windows speaker while >100% boosts DSP!
				_, masterDspBoost := splitHybridVolume(e.GetMasterVolume())
				if e.IsMasterMuted() {
					masterDspBoost = 0.0
				}

				isSilent := (dwFlags&wca.AUDCLNT_BUFFERFLAGS_SILENT) != 0 || data == nil
				if isSilent {
					clear(stereoScratch[:frames*2])
				} else {
					decodeToStereoFloat(data, frames, channels, blockAlign, bitsPerSample, masterDspBoost, stereoScratch)
				}

				e.computeTelemetry(stereoScratch[:frames*2], sampleRate, uint32(channels), uint32(bitsPerSample), periodMs)

				e.mu.RLock()
				for _, target := range e.Targets {
					if target.IsActive() && target.Device.ID != deviceID {
						target.Ring.WriteStereo(stereoScratch[:frames*2], uint64(frames))
					}
				}
				e.mu.RUnlock()
			}

			_ = acc.ReleaseBuffer(framesToRead)
		}
	}
}

func (e *AudioEngine) renderTargetThread(target *TargetState, stopCh chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	revertMMCSS := boostProAudioThread()
	defer revertMMCSS()

	_ = ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED)
	defer ole.CoUninitialize()

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &mmde); err != nil {
		return
	}
	defer mmde.Release()

	dev := getDeviceByID(mmde, target.Device.ID)
	if dev == nil {
		return
	}
	defer dev.Release()

	var ac *wca.IAudioClient
	if err := dev.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &ac); err != nil {
		return
	}
	defer ac.Release()

	var wfx *wca.WAVEFORMATEX
	if err := ac.GetMixFormat(&wfx); err != nil || wfx == nil {
		return
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(wfx)))

	channels := int(wfx.NChannels)
	sampleRate := uint32(wfx.NSamplesPerSec)
	blockAlign := int(wfx.NBlockAlign)
	bitsPerSample := int(wfx.WBitsPerSample)

	var defaultPeriod, minPeriod wca.REFERENCE_TIME
	_ = ac.GetDevicePeriod(&defaultPeriod, &minPeriod)
	if minPeriod == 0 {
		minPeriod = 30000
	}
	if defaultPeriod == 0 {
		defaultPeriod = 100000
	}

	flags := uint32(wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK)
	if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags|AUDCLNT_STREAMFLAGS_LOWLATENCY, minPeriod, 0, wfx, nil); err != nil {
		if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, defaultPeriod, 0, wfx, nil); err != nil {
			return
		}
	}

	hEvent, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return
	}
	defer windows.CloseHandle(hEvent)

	if err := ac.SetEventHandle(uintptr(hEvent)); err != nil {
		return
	}

	var bufferFrames uint32
	if err := ac.GetBufferSize(&bufferFrames); err != nil {
		return
	}

	var arc *wca.IAudioRenderClient
	if err := ac.GetService(wca.IID_IAudioRenderClient, &arc); err != nil {
		return
	}
	defer arc.Release()

	atomic.StoreUint32(&target.SampleRate, sampleRate)
	atomic.StoreUint32(&target.Channels, uint32(channels))
	atomic.StoreUint32(&target.Bits, uint32(bitsPerSample))
	if sampleRate > 0 {
		bufUs := uint32(uint64(bufferFrames) * 1000000 / uint64(sampleRate))
		atomic.StoreUint32(&target.BufferUs, bufUs)
	}

	// Ensure Windows native endpoint volume matches the target slider on stream start
	osScalar, _ := splitHybridVolume(target.GetVolume())
	e.queueOSVolumeUpdate(target.Device.ID, true, osScalar, true, target.IsMuted())

	if err := ac.Start(); err != nil {
		return
	}
	defer ac.Stop()

	const maxScratchFrames = 16384
	readScratch := make([]float32, maxScratchFrames*2)

	for {
		select {
		case <-stopCh:
			return
		default:
		}

		waitRes, _ := windows.WaitForSingleObject(hEvent, 100)
		if waitRes != windows.WAIT_OBJECT_0 {
			continue
		}

		var padding uint32
		if err := ac.GetCurrentPadding(&padding); err != nil {
			continue
		}

		if bufferFrames <= padding {
			continue
		}
		framesAvail := bufferFrames - padding
		if framesAvail == 0 {
			continue
		}

		var outData *byte
		if err := arc.GetBuffer(framesAvail, &outData); err != nil {
			continue
		}

		frames := int(framesAvail)
		if frames > maxScratchFrames {
			frames = maxScratchFrames
		}

		// 0%..100% is handled natively by the target device's Windows OS Hardware Volume!
		// dspBoost is 1.0x for [0..100%], and scales up to 2.0x (+6dB) when slider is [100%..150%].
		_, dspBoost := splitHybridVolume(target.GetVolume())
		muted := target.IsMuted()
		floorMs := atomic.LoadUint32(&e.TargetFloorMs)
		targetLatencyFrames := uint64(sampleRate) * uint64(floorMs) / 1000

		got := target.Ring.ReadStereoAdaptive(readScratch[:frames*2], uint64(frames), dspBoost, muted, targetLatencyFrames)
		if got == 0 || outData == nil {
			_ = arc.ReleaseBuffer(framesAvail, wca.AUDCLNT_BUFFERFLAGS_SILENT)
		} else {
			encodeFromStereoFloat(readScratch[:frames*2], outData, frames, channels, blockAlign, bitsPerSample)
			_ = arc.ReleaseBuffer(framesAvail, 0)
		}
	}
}

func getDeviceByID(mmde *wca.IMMDeviceEnumerator, targetID string) *wca.IMMDevice {
	var dc *wca.IMMDeviceCollection
	if err := mmde.EnumAudioEndpoints(wca.ERender, wca.DEVICE_STATE_ACTIVE, &dc); err != nil {
		return nil
	}
	defer dc.Release()

	var count uint32
	_ = dc.GetCount(&count)
	for i := uint32(0); i < count; i++ {
		var dev *wca.IMMDevice
		if err := dc.Item(i, &dev); err == nil && dev != nil {
			var id string
			_ = dev.GetId(&id)
			if id == targetID {
				return dev
			}
			dev.Release()
		}
	}
	return nil
}

func (e *AudioEngine) computeTelemetry(stereo []float32, sr, ch, bits uint32, periodMs float64) {
	frames := len(stereo) / 2
	if frames == 0 {
		return
	}

	var sumL, sumR, peakL, peakR, dotLR float64
	var bands [16]float64

	for i := 0; i < frames; i++ {
		l := float64(stereo[i*2])
		r := float64(stereo[i*2+1])
		absL := math.Abs(l)
		absR := math.Abs(r)

		if absL > peakL {
			peakL = absL
		}
		if absR > peakR {
			peakR = absR
		}
		sumL += l * l
		sumR += r * r
		dotLR += l * r
	}

	monoSampleCount := frames
	if monoSampleCount > 256 {
		monoSampleCount = 256
	}
	if sr == 0 {
		sr = 48000
	}

	for b := 0; b < 16; b++ {
		freq := 45.0 * math.Pow(1.46, float64(b))
		omega := 2.0 * math.Pi * freq / float64(sr)
		coeff := 2.0 * math.Cos(omega)
		var q1, q2 float64
		for i := 0; i < monoSampleCount; i++ {
			win := 0.5 * (1.0 - math.Cos(2.0*math.Pi*float64(i)/float64(monoSampleCount)))
			mono := float64(stereo[i*2]+stereo[i*2+1]) * 0.5 * win
			q0 := coeff*q1 - q2 + mono
			q2 = q1
			q1 = q0
		}
		mag := math.Sqrt(math.Max(0, q1*q1+q2*q2-q1*q2*coeff)) / float64(monoSampleCount)
		db := 20.0 * math.Log10(mag*28.0+1e-6)
		norm := (db + 42.0) / 42.0
		bands[b] = clampFloat64(norm, 0.0, 1.0)
	}

	rmsL := math.Sqrt(sumL / float64(frames))
	rmsR := math.Sqrt(sumR / float64(frames))

	denom := math.Sqrt(sumL * sumR)
	corr := 1.0
	if denom > 1e-9 {
		corr = clampFloat64(dotLR/denom, -1.0, 1.0)
	}

	bal := 0.0
	if rmsL+rmsR > 1e-6 {
		bal = clampFloat64((rmsR-rmsL)/(rmsL+rmsR), -1.0, 1.0)
	}

	e.telemetryMu.Lock()
	defer e.telemetryMu.Unlock()

	e.Telemetry.SampleRate = sr
	e.Telemetry.Channels = ch
	e.Telemetry.Bits = bits
	e.Telemetry.PeriodMs = periodMs
	e.Telemetry.MMCSSActive = true

	e.Telemetry.PeakL = math.Max(peakL, e.Telemetry.PeakL*0.80)
	e.Telemetry.PeakR = math.Max(peakR, e.Telemetry.PeakR*0.80)
	e.Telemetry.PeakHoldL = math.Max(peakL, e.Telemetry.PeakHoldL*0.975)
	e.Telemetry.PeakHoldR = math.Max(peakR, e.Telemetry.PeakHoldR*0.975)
	e.Telemetry.RMSL = rmsL*0.55 + e.Telemetry.RMSL*0.45
	e.Telemetry.RMSR = rmsR*0.55 + e.Telemetry.RMSR*0.45
	e.Telemetry.Correlation = corr*0.3 + e.Telemetry.Correlation*0.7
	e.Telemetry.Balance = bal*0.3 + e.Telemetry.Balance*0.7

	for b := 0; b < 16; b++ {
		e.Telemetry.Spectrum[b] = math.Max(bands[b], e.Telemetry.Spectrum[b]*0.76)
		if bands[b] >= e.Telemetry.SpecHold[b] {
			e.Telemetry.SpecHold[b] = bands[b]
		} else {
			e.Telemetry.SpecHold[b] = math.Max(0, e.Telemetry.SpecHold[b]-0.018)
		}
	}
}

func (e *AudioEngine) decayTelemetry(sr, ch, bits uint32, periodMs float64) {
	e.telemetryMu.Lock()
	defer e.telemetryMu.Unlock()
	e.Telemetry.SampleRate = sr
	e.Telemetry.Channels = ch
	e.Telemetry.Bits = bits
	e.Telemetry.PeriodMs = periodMs
	e.Telemetry.PeakL *= 0.72
	e.Telemetry.PeakR *= 0.72
	e.Telemetry.PeakHoldL *= 0.94
	e.Telemetry.PeakHoldR *= 0.94
	e.Telemetry.RMSL *= 0.72
	e.Telemetry.RMSR *= 0.72
	for b := 0; b < 16; b++ {
		e.Telemetry.Spectrum[b] *= 0.75
		e.Telemetry.SpecHold[b] = math.Max(0, e.Telemetry.SpecHold[b]-0.025)
	}
}

// ============================================================================
// 4. COMPACT BUBBLE TEA UI & HIGH-PRECISION DSP TELEMETRY VISUALIZER
// ============================================================================

type tickMsg time.Time

type model struct {
	engine    *AudioEngine
	cursor    int
	width     int
	height    int
	statusMsg string
}

var (
	paneBorderStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#2A3447")).
			Padding(0, 1)

	activePaneBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#00E5FF")).
				Padding(0, 1)

	headerBrandStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#070A10")).
				Background(lipgloss.Color("#00E5FF")).
				Padding(0, 1)

	pillProAudio = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#070A10")).
			Background(lipgloss.Color("#00FF9C")).
			Padding(0, 1)

	pillWarn = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#070A10")).
			Background(lipgloss.Color("#FFB800")).
			Padding(0, 1)

	pillMute = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#FF2A85")).
			Padding(0, 1)

	pillSubtle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#94A3B8")).
			Background(lipgloss.Color("#1E293B")).
			Padding(0, 1)

	rowSelectedBg = lipgloss.NewStyle().
			Background(lipgloss.Color("#162235")).
			Foreground(lipgloss.Color("#F8FAFC"))

	dimText     = lipgloss.NewStyle().Foreground(lipgloss.Color("#64748B"))
	mutedText   = lipgloss.NewStyle().Foreground(lipgloss.Color("#94A3B8"))
	cyanText    = lipgloss.NewStyle().Foreground(lipgloss.Color("#00E5FF")).Bold(true)
	greenText   = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FF9C")).Bold(true)
	amberText   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB800")).Bold(true)
	magentaText = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF2A85")).Bold(true)
)

func initialModel(eng *AudioEngine) model {
	_ = eng.RefreshDevices()
	eng.RestartSourceCapture(eng.SourceIndex)
	return model{
		engine:    eng,
		statusMsg: "2-Way Windows OS Volume Sync Active (0-100% Native HW + 100-150% DSP Boost)",
	}
}

func (m model) Init() tea.Cmd {
	return tea.Tick(time.Millisecond*33, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case tickMsg:
		return m, tea.Tick(time.Millisecond*33, func(t time.Time) tea.Msg {
			return tickMsg(t)
		})

	case tea.KeyMsg:
		m.engine.mu.RLock()
		devCount := len(m.engine.Devices)
		m.engine.mu.RUnlock()

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}

		case "down", "j":
			if m.cursor < devCount-1 {
				m.cursor++
			}

		case "s", "enter":
			if devCount > 0 {
				m.engine.RestartSourceCapture(m.cursor)
				m.statusMsg = fmt.Sprintf("Switched Loopback Capture Source -> Endpoint #%d", m.cursor+1)
			}

		case " ":
			if devCount > 0 {
				m.engine.mu.RLock()
				dev := m.engine.Devices[m.cursor]
				srcIdx := m.engine.SourceIndex
				m.engine.mu.RUnlock()

				if m.cursor == srcIdx {
					m.statusMsg = "Echo Guard: Cannot mirror loopback directly into active capture source!"
				} else {
					m.engine.ToggleTarget(dev.ID)
					m.statusMsg = fmt.Sprintf("Toggled Render Stream: %s", dev.Name)
				}
			}

		case "m":
			if devCount > 0 {
				m.engine.mu.RLock()
				dev := m.engine.Devices[m.cursor]
				m.engine.mu.RUnlock()
				muted := m.engine.ToggleDeviceMute(dev.ID)
				if muted {
					m.statusMsg = fmt.Sprintf("Muted Windows Native Endpoint: %s", dev.Name)
				} else {
					m.statusMsg = fmt.Sprintf("Unmuted Windows Native Endpoint: %s", dev.Name)
				}
			}

		case "M":
			muted := m.engine.ToggleMasterMute()
			if muted {
				m.statusMsg = "MASTER SOURCE ENDPOINT MUTED (OS + Loopback)"
			} else {
				m.statusMsg = "MASTER SOURCE ENDPOINT UNMUTED (OS + Loopback)"
			}

		case "left", "h":
			m.adjustSelectedVolume(-0.02)

		case "right", "l":
			m.adjustSelectedVolume(+0.02)

		case "[":
			newVol := m.engine.GetMasterVolume() - 0.02
			m.engine.SetMasterVolume(newVol)
			osScalar, dspBoost := splitHybridVolume(m.engine.GetMasterVolume())
			m.statusMsg = fmt.Sprintf("Master Source Vol -> Windows OS: %.0f%% | DSP Boost: %.2fx", osScalar*100, dspBoost)

		case "]":
			newVol := m.engine.GetMasterVolume() + 0.02
			m.engine.SetMasterVolume(newVol)
			osScalar, dspBoost := splitHybridVolume(m.engine.GetMasterVolume())
			m.statusMsg = fmt.Sprintf("Master Source Vol -> Windows OS: %.0f%% | DSP Boost: %.2fx", osScalar*100, dspBoost)

		case "-", "_":
			cur := atomic.LoadUint32(&m.engine.TargetFloorMs)
			if cur > 4 {
				atomic.StoreUint32(&m.engine.TargetFloorMs, cur-2)
				m.statusMsg = fmt.Sprintf("Target Ring Buffer Floor tightened to %d ms", cur-2)
			}

		case "=", "+":
			cur := atomic.LoadUint32(&m.engine.TargetFloorMs)
			if cur < 50 {
				atomic.StoreUint32(&m.engine.TargetFloorMs, cur+2)
				m.statusMsg = fmt.Sprintf("Target Ring Buffer Floor relaxed to %d ms", cur+2)
			}

		case "r":
			_ = m.engine.RefreshDevices()
			m.statusMsg = "Re-enumerated & Synced Active Windows WASAPI Endpoints"
		}
	}

	return m, nil
}

func (m *model) adjustSelectedVolume(delta float32) {
	m.engine.mu.RLock()
	if len(m.engine.Devices) == 0 || m.cursor >= len(m.engine.Devices) {
		m.engine.mu.RUnlock()
		return
	}
	dev := m.engine.Devices[m.cursor]
	t := m.engine.Targets[dev.ID]
	m.engine.mu.RUnlock()

	if t != nil {
		newVol := t.GetVolume() + delta
		m.engine.SetDeviceVolume(dev.ID, newVol)
		actual := t.GetVolume()
		osScalar, dspBoost := splitHybridVolume(actual)
		m.statusMsg = fmt.Sprintf("[%s] Windows OS Vol: %.0f%% | DSP Boost: %.2fx", truncateString(dev.Name, 24), osScalar*100, dspBoost)
	}
}

func (m model) View() string {
	termWidth := m.width
	if termWidth <= 0 {
		termWidth = 122
	}

	m.engine.telemetryMu.Lock()
	tel := m.engine.Telemetry
	m.engine.telemetryMu.Unlock()

	m.engine.mu.RLock()
	devices := m.engine.Devices
	srcIdx := m.engine.SourceIndex
	masterVol := m.engine.GetMasterVolume()
	masterMuted := m.engine.IsMasterMuted()
	floorMs := atomic.LoadUint32(&m.engine.TargetFloorMs)

	masterDb := gainToDbString(effectiveTotalGain(masterVol), masterMuted)
	topHeader := lipgloss.JoinHorizontal(
		lipgloss.Center,
		headerBrandStyle.Render("⚡ WASAPI RT-MATRIX"),
		" ",
		pillProAudio.Render("OS VOL SYNC"),
		" ",
		pillSubtle.Render(fmt.Sprintf("SRC %dHz/%dbit • %.1fms", tel.SampleRate, tel.Bits, tel.PeriodMs)),
		" ",
		pillSubtle.Render(fmt.Sprintf("DRIFT TARGET: %dms", floorMs)),
		" ",
		func() string {
			if masterMuted {
				return pillMute.Render("MASTER MUTED")
			}
			return pillWarn.Render(fmt.Sprintf("SRC VOL %3.0f%% (%s)", masterVol*100, masterDb))
		}(),
	)

	availW := termWidth - 4
	if availW < 84 {
		availW = 84
	}
	leftW := int(float64(availW) * 0.48)
	if leftW < 48 {
		leftW = 48
	}
	if leftW > 66 {
		leftW = 66
	}
	rightW := availW - leftW - 1
	innerLeftW := leftW - 4
	innerRightW := rightW - 4

	var leftLines []string
	colHdr := fmt.Sprintf("  %-5s %-*s %-8s %-11s", "ROLE", innerLeftW-29, "ENDPOINT", "LATENCY", "OS+DSP VOL")
	leftLines = append(leftLines, cyanText.Render("ROUTING MATRIX")+" "+dimText.Render(fmt.Sprintf("(%d endpoints)", len(devices))))
	leftLines = append(leftLines, dimText.Render(colHdr))
	leftLines = append(leftLines, dimText.Render(strings.Repeat("─", innerLeftW)))

	const maxVisibleRows = 8
	startIdx := 0
	if m.cursor >= maxVisibleRows {
		startIdx = m.cursor - maxVisibleRows + 1
	}
	endIdx := startIdx + maxVisibleRows
	if endIdx > len(devices) {
		endIdx = len(devices)
	}

	nameColW := innerLeftW - 29
	if nameColW < 12 {
		nameColW = 12
	}

	var selectedTarget *TargetState
	var selectedDev DeviceInfo

	for i := startIdx; i < endIdx; i++ {
		d := devices[i]
		t := m.engine.Targets[d.ID]
		if i == m.cursor {
			selectedTarget = t
			selectedDev = d
		}

		curGlyph := "  "
		if i == m.cursor {
			curGlyph = cyanText.Render("▸ ")
		}

		var roleTag string
		switch {
		case i == srcIdx:
			roleTag = cyanText.Render("[SRC]")
		case t != nil && t.IsActive() && t.IsMuted():
			roleTag = magentaText.Render("[MUT]")
		case t != nil && t.IsActive():
			roleTag = greenText.Render("[OUT]")
		case t != nil && t.IsMuted():
			roleTag = magentaText.Render("[mut]")
		default:
			roleTag = dimText.Render("[ · ]")
		}

		cleanName := truncateString(d.Name, nameColW)
		paddedName := fmt.Sprintf("%-*s", nameColW, cleanName)
		if i == m.cursor {
			paddedName = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Render(paddedName)
		} else if t != nil && t.IsActive() {
			paddedName = mutedText.Render(paddedName)
		} else {
			paddedName = dimText.Render(paddedName)
		}

		latStr := dimText.Render("   --   ")
		if i == srcIdx {
			latStr = cyanText.Render(fmt.Sprintf("%5.1fms ", tel.PeriodMs))
		} else if t != nil && t.IsActive() {
			sr := atomic.LoadUint32(&t.SampleRate)
			if sr == 0 {
				sr = 48000
			}
			ringMs := float64(t.Ring.AvailableRead()) * 1000.0 / float64(sr)
			hwMs := float64(atomic.LoadUint32(&t.BufferUs)) / 1000.0
			totalMs := ringMs + hwMs*0.5
			if totalMs < 15.0 {
				latStr = greenText.Render(fmt.Sprintf("%5.1fms ", totalMs))
			} else {
				latStr = amberText.Render(fmt.Sprintf("%5.1fms ", totalMs))
			}
		}

		vol := float32(1.0)
		muted := false
		if t != nil {
			vol = t.GetVolume()
			muted = t.IsMuted()
		}
		volStr := renderCompactGain(vol, muted, 6)

		rowLine := fmt.Sprintf("%s%s %s %s %s", curGlyph, roleTag, paddedName, latStr, volStr)
		if i == m.cursor {
			rowLine = rowSelectedBg.Render(rowLine)
		}
		leftLines = append(leftLines, rowLine)
	}

	for i := endIdx - startIdx; i < maxVisibleRows; i++ {
		leftLines = append(leftLines, "")
	}

	leftLines = append(leftLines, dimText.Render(strings.Repeat("─", innerLeftW)))
	if selectedTarget != nil {
		sr := atomic.LoadUint32(&selectedTarget.SampleRate)
		bits := atomic.LoadUint32(&selectedTarget.Bits)
		hwMs := float64(atomic.LoadUint32(&selectedTarget.BufferUs)) / 1000.0
		ur := atomic.LoadUint64(&selectedTarget.Ring.underrun)
		or := atomic.LoadUint64(&selectedTarget.Ring.overrun)
		ringMs := 0.0
		if sr > 0 {
			ringMs = float64(selectedTarget.Ring.AvailableRead()) * 1000.0 / float64(sr)
		}
		if ringMs > 999.9 {
			ringMs = 999.9
		}
		if ur > 9999 {
			ur = 9999
		}
		if or > 9999 {
			or = 9999
		}

		osScalar, dspBoost := splitHybridVolume(selectedTarget.GetVolume())
		dbStr := gainToDbString(effectiveTotalGain(selectedTarget.GetVolume()), selectedTarget.IsMuted())

		inspTitle := fmt.Sprintf("%-*s", innerLeftW-9, truncateString(selectedDev.Name, innerLeftW-9))
		leftLines = append(leftLines, cyanText.Render("INSPECT: ")+mutedText.Render(inspTitle))

		if selectedTarget.IsActive() {
			// Line 1 (Fixed columns): WIN-OS (4) | DSP + dB (17) | FMT (12) -> ~48 chars total (never wraps)
			line1 := fmt.Sprintf(
				"%s %3.0f%%  %s %4.2fx (%s)  %s %5dHz/%2dbit",
				dimText.Render("WIN-OS:"), osScalar*100,
				dimText.Render("DSP:"), dspBoost, amberText.Render(dbStr),
				dimText.Render("FMT:"), sr, bits,
			)
			// Line 2 (Fixed columns): RING (7 chars: "%5.1fms") | DAC (7 chars: "%5.1fms") | U/O (9 chars: "%4d/%-4d")
			line2 := fmt.Sprintf(
				"%s %5.1fms   %s %5.1fms   %s %4d/%-4d",
				dimText.Render("RING:"), ringMs,
				dimText.Render("DAC:"), hwMs,
				dimText.Render("U/O:"), ur, or,
			)
			leftLines = append(leftLines, line1, line2)
		} else {
			line1 := fmt.Sprintf(
				"%s %3.0f%%  %s %4.2fx (%s)  %s STANDBY",
				dimText.Render("WIN-OS:"), osScalar*100,
				dimText.Render("DSP:"), dspBoost, amberText.Render(dbStr),
				dimText.Render("STATE:"),
			)
			line2 := dimText.Render("Press [Space] to enable render stream • [←/→] Adjust OS Vol")
			leftLines = append(leftLines, line1, line2)
		}
	}
	m.engine.mu.RUnlock()

	leftPane := activePaneBorderStyle.Width(leftW).Render(strings.Join(leftLines, "\n"))

	meterBarW := innerRightW - 18
	if meterBarW < 16 {
		meterBarW = 16
	}

	vuL := renderLogDBMeter("L", tel.RMSL, tel.PeakL, tel.PeakHoldL, meterBarW)
	vuR := renderLogDBMeter("R", tel.RMSR, tel.PeakR, tel.PeakHoldR, meterBarW)
	scaleLine := renderDBScaleLine(meterBarW)
	stereoScope := renderStereoPhaseBar(tel.Balance, tel.Correlation, meterBarW)
	specBlock := renderSpectrumWithCaps(tel.Spectrum, tel.SpecHold, 7, innerRightW)

	rightLines := []string{
		cyanText.Render("MASTER DSP TELEMETRY & LOGARITHMIC dBFS METER"),
		dimText.Render(strings.Repeat("─", innerRightW)),
		vuL,
		vuR,
		scaleLine,
		stereoScope,
		dimText.Render(strings.Repeat("─", innerRightW)),
		cyanText.Render("16-BAND HARMONIC RTA SPECTRUM (HANN-GOERTZEL • PEAK HOLD)"),
		specBlock,
	}

	rightPane := paneBorderStyle.Width(rightW).Render(strings.Join(rightLines, "\n"))
	body := lipgloss.JoinHorizontal(lipgloss.Top, leftPane, " ", rightPane)

	statusBar := greenText.Render("● ") + mutedText.Render(m.statusMsg)
	helpBar := dimText.Render(
		"[↑/↓] Nav  [Space] Toggle Out  [Enter/s] Set Src  [←/→] OS Vol+Boost  [m] OS Mute  [[/]] Src Vol  [-/+] Latency  [q] Quit",
	)

	return lipgloss.JoinVertical(
		lipgloss.Left,
		topHeader,
		body,
		statusBar,
		helpBar,
	)
}

func gainToDbString(linearGain float32, muted bool) string {
	if muted || linearGain <= 0.0001 {
		return "  -∞ dB "
	}
	db := 20.0 * math.Log10(float64(linearGain))
	return fmt.Sprintf("%+6.1fdB", db) // Always 8 chars (e.g., " -19.9dB" or "  +0.0dB")
}

func renderCompactGain(slider float32, muted bool, width int) string {
	if muted {
		return magentaText.Render("━━MUT━━   0%")
	}
	norm := clampFloat64(float64(slider)/1.5, 0.0, 1.0)
	exact := norm * float64(width)
	full := int(exact)
	rem := exact - float64(full)

	var bar strings.Builder
	for i := 0; i < width; i++ {
		var seg string
		if i < full {
			seg = "━"
		} else if i == full && rem >= 0.4 {
			seg = "╸"
		} else {
			seg = "─"
		}
		if i >= int(float64(width)*0.67) && i <= full {
			bar.WriteString(amberText.Render(seg))
		} else if i <= full {
			bar.WriteString(cyanText.Render(seg))
		} else {
			bar.WriteString(dimText.Render(seg))
		}
	}
	return fmt.Sprintf("%s %3.0f%%", bar.String(), slider*100)
}

func ampToDbNorm(amp float64) (float64, float64) {
	db := 20.0 * math.Log10(amp+1e-6)
	if db < -60.0 {
		db = -60.0
	}
	if db > 3.0 {
		db = 3.0
	}
	norm := clampFloat64((db+60.0)/60.0, 0.0, 1.0)
	return db, norm
}

func renderLogDBMeter(label string, rms, peak, hold float64, width int) string {
	_, rmsNorm := ampToDbNorm(rms)
	peakDb, peakNorm := ampToDbNorm(peak)
	_, holdNorm := ampToDbNorm(hold)

	rmsCell := int(rmsNorm * float64(width))
	peakCell := int(peakNorm * float64(width))
	holdCell := int(holdNorm * float64(width))
	if holdCell >= width {
		holdCell = width - 1
	}

	var sb strings.Builder
	for i := 0; i < width; i++ {
		ratio := float64(i) / float64(width)
		var color lipgloss.Color
		switch {
		case ratio >= 0.88:
			color = lipgloss.Color("#FF2A85")
		case ratio >= 0.70:
			color = lipgloss.Color("#FFB800")
		case ratio >= 0.45:
			color = lipgloss.Color("#00FF9C")
		default:
			color = lipgloss.Color("#00E5FF")
		}

		switch {
		case i == holdCell && hold > 0.005:
			sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#FFFFFF")).Bold(true).Render("┃"))
		case i < rmsCell:
			sb.WriteString(lipgloss.NewStyle().Foreground(color).Render("█"))
		case i < peakCell:
			sb.WriteString(lipgloss.NewStyle().Foreground(color).Render("▒"))
		default:
			sb.WriteString(dimText.Render("·"))
		}
	}

	dbStyle := mutedText
	if peakDb > -1.0 {
		dbStyle = magentaText
	} else if peakDb > -6.0 {
		dbStyle = amberText
	}
	return fmt.Sprintf("%s %s %s", cyanText.Render(label), sb.String(), dbStyle.Render(fmt.Sprintf("%+5.1f dBFS", peakDb)))
}

func renderDBScaleLine(width int) string {
	marks := []struct {
		db  int
		lbl string
	}{
		{-60, "-60"},
		{-40, "-40"},
		{-24, "-24"},
		{-12, "-12"},
		{-6, "-6"},
		{0, "0dB"},
	}
	buf := make([]rune, width)
	for i := range buf {
		buf[i] = ' '
	}
	for _, m := range marks {
		pos := int(float64(m.db+60) / 60.0 * float64(width-1))
		runes := []rune(m.lbl)
		if pos+len(runes) >= width {
			pos = width - len(runes)
		}
		if pos >= 0 {
			copy(buf[pos:], runes)
		}
	}
	return "  " + dimText.Render(string(buf))
}

func renderStereoPhaseBar(balance, corr float64, width int) string {
	halfW := (width - 14) / 2
	if halfW < 8 {
		halfW = 8
	}

	balIdx := int((balance + 1.0) * 0.5 * float64(halfW-1))
	if balIdx < 0 {
		balIdx = 0
	} else if balIdx >= halfW {
		balIdx = halfW - 1
	}
	var balSb strings.Builder
	for i := 0; i < halfW; i++ {
		if i == balIdx {
			balSb.WriteString(cyanText.Render("◆"))
		} else if i == halfW/2 {
			balSb.WriteString(mutedText.Render("┼"))
		} else {
			balSb.WriteString(dimText.Render("─"))
		}
	}

	corrIdx := int((corr + 1.0) * 0.5 * float64(halfW-1))
	if corrIdx < 0 {
		corrIdx = 0
	} else if corrIdx >= halfW {
		corrIdx = halfW - 1
	}
	var corrSb strings.Builder
	for i := 0; i < halfW; i++ {
		if i == corrIdx {
			if corr < 0 {
				corrSb.WriteString(magentaText.Render("●"))
			} else {
				corrSb.WriteString(greenText.Render("●"))
			}
		} else if i == halfW/2 {
			corrSb.WriteString(mutedText.Render("┼"))
		} else {
			corrSb.WriteString(dimText.Render("─"))
		}
	}

	return fmt.Sprintf("%s L%sR   %s -1%s+1",
		dimText.Render("PAN"), balSb.String(),
		dimText.Render("PHASE"), corrSb.String(),
	)
}

func renderSpectrumWithCaps(bands [16]float64, holds [16]float64, rows int, innerWidth int) string {
	blocks := []string{" ", "▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}
	colW := 2
	gap := " "

	var lines []string
	for r := rows - 1; r >= 0; r-- {
		var rowBuilder strings.Builder
		for b := 0; b < 16; b++ {
			level := bands[b] * float64(rows)
			holdRow := int(holds[b] * float64(rows))
			if holdRow >= rows {
				holdRow = rows - 1
			}
			rem := level - float64(r)

			var cell string
			var color string

			if r >= rows-1 {
				color = "#FF2A85"
			} else if r >= rows-3 {
				color = "#FFB800"
			} else if r >= rows-5 {
				color = "#00FF9C"
			} else {
				color = "#00E5FF"
			}

			switch {
			case rem >= 1.0:
				cell = strings.Repeat("█", colW)
				rowBuilder.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(cell) + gap)
			case rem > 0.0:
				idx := int(rem * 8.0)
				if idx > 8 {
					idx = 8
				}
				cell = strings.Repeat(blocks[idx], colW)
				rowBuilder.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(cell) + gap)
			case r == holdRow && holds[b] > 0.06:
				cell = strings.Repeat("▔", colW)
				rowBuilder.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#E2E8F0")).Render(cell) + gap)
			default:
				cell = strings.Repeat("·", colW)
				rowBuilder.WriteString(dimText.Render(cell) + gap)
			}
		}
		lines = append(lines, rowBuilder.String())
	}

	freqLabels := []string{"45", "65", "95", "140", "205", "300", "440", "640", "930", "1.3k", "2k", "2.9k", "4.2k", "6.2k", "9k", "13k"}
	var axisBuilder strings.Builder
	for _, fl := range freqLabels {
		if len(fl) > colW {
			fl = fl[:colW]
		}
		axisBuilder.WriteString(fmt.Sprintf("%-*s%s", colW, fl, gap))
	}
	lines = append(lines, dimText.Render(axisBuilder.String()))

	return strings.Join(lines, "\n")
}

func truncateString(s string, maxLen int) string {
	if maxLen <= 1 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}

func main() {
	engine := NewAudioEngine()
	p := tea.NewProgram(initialModel(engine), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Fatal error: %v\n", err)
		os.Exit(1)
	}
}
