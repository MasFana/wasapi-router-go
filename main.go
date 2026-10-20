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
	"time"
	"unsafe"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
)

// ============================================================================
// 1. LOCK-FREE SPSC RING BUFFER (Heap-Safe Stereo Float32)
// ============================================================================

type SpscRingBuffer struct {
	buf      []float32
	capacity uint64
	writePos uint64 // atomic
	readPos  uint64 // atomic
	underrun uint64 // atomic
	overrun  uint64 // atomic
}

func NewSpscRingBuffer(frameCapacity uint64) *SpscRingBuffer {
	return &SpscRingBuffer{
		buf:      make([]float32, frameCapacity*2), // Always normalized to stereo float32 internally
		capacity: frameCapacity,
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

func (r *SpscRingBuffer) WriteStereo(src []float32, frames uint64) uint64 {
	avail := r.AvailableWrite()
	if frames > avail {
		atomic.AddUint64(&r.overrun, 1)
		frames = avail
	}
	if frames == 0 {
		return 0
	}

	w := atomic.LoadUint64(&r.writePos)
	for i := uint64(0); i < frames; i++ {
		idx := ((w + i) % r.capacity) * 2
		srcIdx := i * 2
		r.buf[idx] = src[srcIdx]
		r.buf[idx+1] = src[srcIdx+1]
	}

	atomic.StoreUint64(&r.writePos, w+frames)
	return frames
}

func (r *SpscRingBuffer) ReadStereo(dst []float32, frames uint64, gain float32, muted bool) uint64 {
	avail := r.AvailableRead()
	if frames > avail {
		atomic.AddUint64(&r.underrun, 1)
		for i := uint64(0); i < frames*2; i++ {
			dst[i] = 0
		}
		return 0
	}

	rd := atomic.LoadUint64(&r.readPos)
	effectiveGain := gain
	if muted {
		effectiveGain = 0.0
	}

	for i := uint64(0); i < frames; i++ {
		idx := ((rd + i) % r.capacity) * 2
		dstIdx := i * 2
		dst[dstIdx] = r.buf[idx] * effectiveGain
		dst[dstIdx+1] = r.buf[idx+1] * effectiveGain
	}

	atomic.StoreUint64(&r.readPos, rd+frames)
	return frames
}

// ============================================================================
// 2. SAFE PCM DECODER / ENCODER (Prevents 0xc0000374 Heap Overflow)
// ============================================================================

// decodeToStereoFloat membaca raw byte buffer WASAPI sesuai nBlockAlign & wBitsPerSample
// sehingga tidak pernah membaca di luar batas memori yang dialokasikan oleh Windows Audio Engine.
func decodeToStereoFloat(rawPtr *byte, frames int, channels int, blockAlign int, bitsPerSample int, masterGain float32, dstStereo []float32) {
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

		if bitsPerSample == 32 && bytesPerSample == 4 {
			lBits := binary.LittleEndian.Uint32(rawBytes[frameOffset : frameOffset+4])
			l = math.Float32frombits(lBits)
			if channels > 1 && frameOffset+8 <= totalBytes {
				rBits := binary.LittleEndian.Uint32(rawBytes[frameOffset+4 : frameOffset+8])
				r = math.Float32frombits(rBits)
			} else {
				r = l
			}
		} else if bitsPerSample == 16 && bytesPerSample == 2 {
			lInt := int16(binary.LittleEndian.Uint16(rawBytes[frameOffset : frameOffset+2]))
			l = float32(lInt) / 32768.0
			if channels > 1 && frameOffset+4 <= totalBytes {
				rInt := int16(binary.LittleEndian.Uint16(rawBytes[frameOffset+2 : frameOffset+4]))
				r = float32(rInt) / 32768.0
			} else {
				r = l
			}
		}

		// Clamp extreme spikes
		if math.IsNaN(float64(l)) || math.IsInf(float64(l), 0) {
			l = 0
		}
		if math.IsNaN(float64(r)) || math.IsInf(float64(r), 0) {
			r = 0
		}

		dstStereo[f*2] = l * masterGain
		dstStereo[f*2+1] = r * masterGain
	}
}

// encodeFromStereoFloat menulis stereo float32 ke raw byte buffer WASAPI sesuai ukuran exact frame
// untuk mencegah penulisan lewat batas akhir heap (Heap Corruption 0xc0000374).
func encodeFromStereoFloat(srcStereo []float32, rawPtr *byte, frames int, channels int, blockAlign int, bitsPerSample int) {
	if rawPtr == nil || frames <= 0 || blockAlign <= 0 {
		return
	}

	totalBytes := frames * blockAlign
	outBytes := unsafe.Slice(rawPtr, totalBytes)
	// Bersihkan buffer terlebih dahulu
	for i := range outBytes {
		outBytes[i] = 0
	}

	bytesPerSample := blockAlign / channels
	if bytesPerSample <= 0 {
		return
	}

	for f := 0; f < frames; f++ {
		l := srcStereo[f*2]
		r := srcStereo[f*2+1]
		if l > 1.0 {
			l = 1.0
		} else if l < -1.0 {
			l = -1.0
		}
		if r > 1.0 {
			r = 1.0
		} else if r < -1.0 {
			r = -1.0
		}

		frameOffset := f * blockAlign

		if bitsPerSample == 32 && bytesPerSample == 4 {
			binary.LittleEndian.PutUint32(outBytes[frameOffset:frameOffset+4], math.Float32bits(l))
			if channels > 1 && frameOffset+8 <= totalBytes {
				binary.LittleEndian.PutUint32(outBytes[frameOffset+4:frameOffset+8], math.Float32bits(r))
			}
			// Fill extra surround channels if target is 5.1/7.1
			for c := 2; c < channels; c++ {
				off := frameOffset + c*4
				if off+4 <= totalBytes {
					mid := (l + r) * 0.35
					binary.LittleEndian.PutUint32(outBytes[off:off+4], math.Float32bits(mid))
				}
			}
		} else if bitsPerSample == 16 && bytesPerSample == 2 {
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
// 3. AUDIO ENGINE & WASAPI LOOPBACK / RENDER PIPELINE
// ============================================================================

type DeviceInfo struct {
	ID        string
	Name      string
	IsDefault bool
}

type TargetState struct {
	Device     DeviceInfo
	Active     bool
	Muted      bool
	Volume     float32
	Ring       *SpscRingBuffer
	StopChan   chan struct{}
	SampleRate uint32
	Channels   uint32
	Bits       uint32
	BufferMs   float64
}

type AudioTelemetry struct {
	PeakL      float64
	PeakR      float64
	RMSL       float64
	RMSR       float64
	Spectrum   [16]float64
	SampleRate uint32
	Channels   uint32
	Bits       uint32
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

	telemetryMu sync.Mutex
	Telemetry   AudioTelemetry
}

func NewAudioEngine() *AudioEngine {
	eng := &AudioEngine{
		Targets:     make(map[string]*TargetState),
		SourceIndex: 0,
	}
	eng.SetMasterVolume(1.0)
	return eng
}

func (e *AudioEngine) SetMasterVolume(v float32) {
	if v < 0 {
		v = 0
	} else if v > 1.5 {
		v = 1.5
	}
	atomic.StoreUint32(&e.MasterVolBits, math.Float32bits(v))
}

func (e *AudioEngine) GetMasterVolume() float32 {
	return math.Float32frombits(atomic.LoadUint32(&e.MasterVolBits))
}

func (e *AudioEngine) ToggleMasterMute() bool {
	cur := atomic.LoadUint32(&e.MasterMute)
	next := uint32(1)
	if cur == 1 {
		next = 0
	}
	atomic.StoreUint32(&e.MasterMute, next)
	return next == 1
}

func (e *AudioEngine) IsMasterMuted() bool {
	return atomic.LoadUint32(&e.MasterMute) == 1
}

// RefreshDevices runs inside an isolated short-lived worker goroutine to avoid corrupting BubbleTea's main thread COM state
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

		var list []DeviceInfo
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
			dev.Release()

			list = append(list, DeviceInfo{
				ID:        id,
				Name:      name,
				IsDefault: id == defaultID,
			})
		}

		e.mu.Lock()
		e.Devices = list
		for idx, d := range list {
			if d.IsDefault && !e.IsCapturing {
				e.SourceIndex = idx
			}
			if _, exists := e.Targets[d.ID]; !exists {
				e.Targets[d.ID] = &TargetState{
					Device: d,
					Volume: 1.0,
					Ring:   NewSpscRingBuffer(96000),
				}
			}
		}
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

	if t.Active {
		if t.StopChan != nil {
			close(t.StopChan)
		}
		t.Active = false
	} else {
		atomic.StoreUint64(&t.Ring.writePos, 0)
		atomic.StoreUint64(&t.Ring.readPos, 0)
		t.StopChan = make(chan struct{})
		t.Active = true
		go e.renderTargetThread(t, t.StopChan)
	}
}

func (e *AudioEngine) captureLoopbackThread(deviceID string, stopCh chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

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
	// Copy format values immediately before Initialize, free pointer safely on exit
	channels := int(wfx.NChannels)
	sampleRate := uint32(wfx.NSamplesPerSec)
	blockAlign := int(wfx.NBlockAlign)
	bitsPerSample := int(wfx.WBitsPerSample)

	var defaultPeriod, minPeriod wca.REFERENCE_TIME
	_ = ac.GetDevicePeriod(&defaultPeriod, &minPeriod)
	if defaultPeriod == 0 {
		defaultPeriod = 100000 // 10ms fallback in 100ns units
	}

	flags := uint32(wca.AUDCLNT_STREAMFLAGS_LOOPBACK | wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK)
	if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, defaultPeriod, 0, wfx, nil); err != nil {
		return
	}

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

		waitRes, _ := windows.WaitForSingleObject(hEvent, 120)
		if waitRes != windows.WAIT_OBJECT_0 {
			e.decayTelemetry(sampleRate, uint32(channels), uint32(bitsPerSample))
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

			// IMPORTANT: Pass valid non-nil pointers for devPos & qpcPos to avoid bad COM pointer dereference
			if err := acc.GetBuffer(&data, &framesToRead, &dwFlags, &devPos, &qpcPos); err != nil {
				break
			}

			if framesToRead > 0 {
				frames := int(framesToRead)
				if frames > maxScratchFrames {
					frames = maxScratchFrames
				}

				masterGain := e.GetMasterVolume()
				if e.IsMasterMuted() {
					masterGain = 0.0
				}

				isSilent := (dwFlags&wca.AUDCLNT_BUFFERFLAGS_SILENT) != 0 || data == nil
				if isSilent {
					for i := 0; i < frames*2; i++ {
						stereoScratch[i] = 0
					}
				} else {
					decodeToStereoFloat(data, frames, channels, blockAlign, bitsPerSample, masterGain, stereoScratch)
				}

				e.computeTelemetry(stereoScratch[:frames*2], sampleRate, uint32(channels), uint32(bitsPerSample))

				e.mu.RLock()
				for _, target := range e.Targets {
					if target.Active && target.Device.ID != deviceID {
						maxTargetFrames := uint64(sampleRate / 20) // ~50ms drift ceiling
						if target.Ring.AvailableRead() > maxTargetFrames {
							atomic.AddUint64(&target.Ring.readPos, uint64(frames))
						}
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

	channels := int(wfx.NChannels)
	sampleRate := uint32(wfx.NSamplesPerSec)
	blockAlign := int(wfx.NBlockAlign)
	bitsPerSample := int(wfx.WBitsPerSample)

	var defaultPeriod, minPeriod wca.REFERENCE_TIME
	_ = ac.GetDevicePeriod(&defaultPeriod, &minPeriod)
	if defaultPeriod == 0 {
		defaultPeriod = 100000
	}

	flags := uint32(wca.AUDCLNT_STREAMFLAGS_EVENTCALLBACK)
	if err := ac.Initialize(wca.AUDCLNT_SHAREMODE_SHARED, flags, defaultPeriod, 0, wfx, nil); err != nil {
		return
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

	target.SampleRate = sampleRate
	target.Channels = uint32(channels)
	target.Bits = uint32(bitsPerSample)
	if sampleRate > 0 {
		target.BufferMs = float64(bufferFrames) * 1000.0 / float64(sampleRate)
	}

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

		waitRes, _ := windows.WaitForSingleObject(hEvent, 150)
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

		e.mu.RLock()
		vol := target.Volume
		muted := target.Muted
		e.mu.RUnlock()

		got := target.Ring.ReadStereo(readScratch[:frames*2], uint64(frames), vol, muted)
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

func (e *AudioEngine) computeTelemetry(stereo []float32, sr, ch, bits uint32) {
	frames := len(stereo) / 2
	if frames == 0 {
		return
	}

	var sumL, sumR, peakL, peakR float64
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
	}

	monoSampleCount := frames
	if monoSampleCount > 256 {
		monoSampleCount = 256
	}
	if sr == 0 {
		sr = 48000
	}
	for b := 0; b < 16; b++ {
		freq := 50.0 * math.Pow(1.32, float64(b))
		omega := 2.0 * math.Pi * freq / float64(sr)
		coeff := 2.0 * math.Cos(omega)
		var q1, q2 float64
		for i := 0; i < monoSampleCount; i++ {
			mono := float64(stereo[i*2]+stereo[i*2+1]) * 0.5
			q0 := coeff*q1 - q2 + mono
			q2 = q1
			q1 = q0
		}
		mag := math.Sqrt(math.Max(0, q1*q1+q2*q2-q1*q2*coeff)) / float64(monoSampleCount)
		bands[b] = math.Min(1.0, mag*18.0)
	}

	rmsL := math.Sqrt(sumL / float64(frames))
	rmsR := math.Sqrt(sumR / float64(frames))

	e.telemetryMu.Lock()
	defer e.telemetryMu.Unlock()

	e.Telemetry.SampleRate = sr
	e.Telemetry.Channels = ch
	e.Telemetry.Bits = bits
	e.Telemetry.PeakL = math.Max(peakL, e.Telemetry.PeakL*0.82)
	e.Telemetry.PeakR = math.Max(peakR, e.Telemetry.PeakR*0.82)
	e.Telemetry.RMSL = rmsL*0.6 + e.Telemetry.RMSL*0.4
	e.Telemetry.RMSR = rmsR*0.6 + e.Telemetry.RMSR*0.4

	for b := 0; b < 16; b++ {
		e.Telemetry.Spectrum[b] = math.Max(bands[b], e.Telemetry.Spectrum[b]*0.78)
	}
}

func (e *AudioEngine) decayTelemetry(sr, ch, bits uint32) {
	e.telemetryMu.Lock()
	defer e.telemetryMu.Unlock()
	e.Telemetry.SampleRate = sr
	e.Telemetry.Channels = ch
	e.Telemetry.Bits = bits
	e.Telemetry.PeakL *= 0.75
	e.Telemetry.PeakR *= 0.75
	e.Telemetry.RMSL *= 0.75
	e.Telemetry.RMSR *= 0.75
	for b := 0; b < 16; b++ {
		e.Telemetry.Spectrum[b] *= 0.75
	}
}

// ============================================================================
// 4. BUBBLE TEA RICH UI & REAL-TIME VISUALIZER
// ============================================================================

type tickMsg time.Time

type model struct {
	engine    *AudioEngine
	cursor    int
	focusPane int
	width     int
	height    int
	statusMsg string
}

var (
	bgStyle = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#00E5FF")).
		Padding(0, 1)

	activePaneStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#00FF9C")).
			Padding(0, 1)

	inactivePaneStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#3B4252")).
				Padding(0, 1)

	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#0B0F19")).
			Background(lipgloss.Color("#00E5FF")).
			Padding(0, 2)

	badgeActive = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#0B0F19")).
			Background(lipgloss.Color("#00FF9C")).
			Padding(0, 1)

	badgeMuted = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#FF2A85")).
			Padding(0, 1)

	subTextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#6C7A89"))
	cyanText     = lipgloss.NewStyle().Foreground(lipgloss.Color("#00E5FF")).Bold(true)
	greenText    = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FF9C")).Bold(true)
	warnText     = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB800")).Bold(true)
)

func initialModel(eng *AudioEngine) model {
	_ = eng.RefreshDevices()
	eng.RestartSourceCapture(eng.SourceIndex)
	return model{
		engine:    eng,
		focusPane: 1,
		statusMsg: "Heap-Safe WASAPI Loopback Active | Dynamic BlockAlign PCM Engine",
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

		case "tab":
			m.focusPane = (m.focusPane + 1) % 2

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
				m.statusMsg = fmt.Sprintf("Switched Loopback Capture Source to Endpoint #%d", m.cursor)
			}

		case " ":
			if devCount > 0 {
				m.engine.mu.RLock()
				dev := m.engine.Devices[m.cursor]
				srcIdx := m.engine.SourceIndex
				m.engine.mu.RUnlock()

				if m.cursor == srcIdx {
					m.statusMsg = "Echo Guard: Cannot mirror loopback directly into the same source endpoint!"
				} else {
					m.engine.ToggleTarget(dev.ID)
					m.statusMsg = fmt.Sprintf("Toggled WASAPI Render Stream for: %s", dev.Name)
				}
			}

		case "m":
			if devCount > 0 {
				m.engine.mu.Lock()
				dev := m.engine.Devices[m.cursor]
				if t, ok := m.engine.Targets[dev.ID]; ok {
					t.Muted = !t.Muted
				}
				m.engine.mu.Unlock()
			}

		case "M":
			muted := m.engine.ToggleMasterMute()
			if muted {
				m.statusMsg = "MASTER LOOPBACK MUTED"
			} else {
				m.statusMsg = "MASTER LOOPBACK UNMUTED"
			}

		case "left", "h":
			m.adjustSelectedVolume(-0.05)

		case "right", "l":
			m.adjustSelectedVolume(+0.05)

		case "[":
			m.engine.SetMasterVolume(m.engine.GetMasterVolume() - 0.05)

		case "]":
			m.engine.SetMasterVolume(m.engine.GetMasterVolume() + 0.05)

		case "r":
			_ = m.engine.RefreshDevices()
			m.statusMsg = "Re-enumerated Active Windows WASAPI Render Endpoints"
		}
	}

	return m, nil
}

func (m *model) adjustSelectedVolume(delta float32) {
	m.engine.mu.Lock()
	defer m.engine.mu.Unlock()
	if len(m.engine.Devices) == 0 || m.cursor >= len(m.engine.Devices) {
		return
	}
	dev := m.engine.Devices[m.cursor]
	if t, ok := m.engine.Targets[dev.ID]; ok {
		v := t.Volume + delta
		if v < 0.0 {
			v = 0.0
		} else if v > 1.5 {
			v = 1.5
		}
		t.Volume = v
	}
}

func (m model) View() string {
	m.engine.telemetryMu.Lock()
	tel := m.engine.Telemetry
	m.engine.telemetryMu.Unlock()

	m.engine.mu.RLock()
	devices := m.engine.Devices
	srcIdx := m.engine.SourceIndex
	masterVol := m.engine.GetMasterVolume()
	masterMuted := m.engine.IsMasterMuted()

	header := lipgloss.JoinHorizontal(
		lipgloss.Center,
		titleStyle.Render(" WASAPI LOW-LATENCY ROUTER & DSP TELEMETRY "),
		"  ",
		cyanText.Render(fmt.Sprintf("SOURCE: %d Hz / %d Ch / %d-bit", tel.SampleRate, tel.Channels, tel.Bits)),
		"  ",
		func() string {
			if masterMuted {
				return badgeMuted.Render("MASTER MUTE")
			}
			return badgeActive.Render(fmt.Sprintf("MASTER GAIN: %3.0f%%", masterVol*100))
		}(),
	)

	var devRows []string
	devRows = append(devRows, cyanText.Render("ENDPOINT MATRIX (Source Loopback -> Multi-Target SPSC Render)"))
	devRows = append(devRows, subTextStyle.Render(strings.Repeat("─", 68)))

	for i, d := range devices {
		cursorPrefix := "  "
		if i == m.cursor {
			cursorPrefix = "▸ "
		}

		srcBadge := "      "
		if i == srcIdx {
			srcBadge = badgeActive.Render("SOURCE")
		}

		t := m.engine.Targets[d.ID]
		tgtBadge := subTextStyle.Render("[BYPASS]")
		ringInfo := ""

		if t != nil && t.Active {
			if t.Muted {
				tgtBadge = badgeMuted.Render("MUTED ")
			} else {
				tgtBadge = greenText.Render("[RENDER]")
			}
			fillFrames := t.Ring.AvailableRead()
			sr := t.SampleRate
			if sr == 0 {
				sr = 48000
			}
			fillMs := float64(fillFrames) * 1000.0 / float64(sr)
			ur := atomic.LoadUint64(&t.Ring.underrun)
			or := atomic.LoadUint64(&t.Ring.overrun)
			ringInfo = fmt.Sprintf("  ↳ Ring: %4.1fms | %dHz/%dbit | Buf: %4.1fms | U:%d O:%d",
				fillMs, t.SampleRate, t.Bits, t.BufferMs, ur, or)
		}

		volBar := renderMiniSlider(t.Volume, 12)
		nameStr := truncateString(d.Name, 28)
		if i == m.cursor {
			nameStr = cyanText.Render(nameStr)
		}

		row := fmt.Sprintf("%s%-6s %-8s %-28s %s %3.0f%%",
			cursorPrefix, srcBadge, tgtBadge, nameStr, volBar, t.Volume*100)
		devRows = append(devRows, row)
		if ringInfo != "" {
			devRows = append(devRows, warnText.Render(ringInfo))
		}
	}
	m.engine.mu.RUnlock()

	leftPaneStyle := activePaneStyle
	if m.focusPane != 1 {
		leftPaneStyle = inactivePaneStyle
	}
	leftPane := leftPaneStyle.Width(72).Render(strings.Join(devRows, "\n"))

	vuL := renderVUMeter("L", tel.RMSL, tel.PeakL, 28)
	vuR := renderVUMeter("R", tel.RMSR, tel.PeakR, 28)
	spec := renderSpectrum16(tel.Spectrum, 8)

	rightContent := strings.Join([]string{
		cyanText.Render("REAL-TIME PCM VISUALIZER"),
		subTextStyle.Render(strings.Repeat("─", 40)),
		vuL,
		vuR,
		"",
		cyanText.Render("16-BAND FREQUENCY SPECTRUM (50Hz - 16kHz)"),
		spec,
	}, "\n")

	rightPane := bgStyle.Width(44).Render(rightContent)
	mainBody := lipgloss.JoinHorizontal(lipgloss.Top, leftPane, " ", rightPane)

	helpBar := subTextStyle.Render(
		"[↑/↓] Select  [Enter/s] Set Capture Source  [Space] Toggle Target Render  [←/→] Target Vol  [m] Mute Target  [[/]] Master Vol  [M] Master Mute  [r] Refresh  [q] Quit",
	)
	statusLine := greenText.Render("STATUS: ") + m.statusMsg

	return lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		"",
		mainBody,
		"",
		statusLine,
		helpBar,
	)
}

func renderMiniSlider(val float32, width int) string {
	norm := float64(val) / 1.5
	if norm > 1.0 {
		norm = 1.0
	}
	filled := int(math.Round(norm * float64(width)))
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < width; i++ {
		if i < filled {
			sb.WriteString("━")
		} else {
			sb.WriteString("─")
		}
	}
	sb.WriteString("]")
	return sb.String()
}

func renderVUMeter(chLabel string, rms, peak float64, width int) string {
	rmsCells := int(math.Min(1.0, rms*1.8) * float64(width))
	peakCell := int(math.Min(1.0, peak*1.4) * float64(width))
	if peakCell >= width {
		peakCell = width - 1
	}

	var bar strings.Builder
	for i := 0; i < width; i++ {
		var cell string
		if i == peakCell && peak > 0.02 {
			cell = "▎"
		} else if i < rmsCells {
			cell = "█"
		} else {
			cell = "░"
		}

		ratio := float64(i) / float64(width)
		if ratio > 0.85 {
			bar.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#FF2A85")).Render(cell))
		} else if ratio > 0.65 {
			bar.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB800")).Render(cell))
		} else {
			bar.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#00FF9C")).Render(cell))
		}
	}

	db := 20.0 * math.Log10(peak+1e-6)
	if db < -60.0 {
		db = -60.0
	}
	return fmt.Sprintf("%s %s %5.1f dB", cyanText.Render(chLabel), bar.String(), db)
}

func renderSpectrum16(bands [16]float64, rows int) string {
	blocks := []string{" ", "▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}
	var lines []string

	for r := rows - 1; r >= 0; r-- {
		var rowBuilder strings.Builder
		for b := 0; b < 16; b++ {
			level := bands[b] * float64(rows)
			rem := level - float64(r)
			var glyph string
			if rem >= 1.0 {
				glyph = "██"
			} else if rem <= 0.0 {
				glyph = "░░"
			} else {
				idx := int(rem * 8.0)
				if idx > 8 {
					idx = 8
				}
				glyph = blocks[idx] + blocks[idx]
			}

			color := "#00E5FF"
			if r >= rows-2 {
				color = "#FF2A85"
			} else if r >= rows-4 {
				color = "#00FF9C"
			}
			rowBuilder.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(glyph) + " ")
		}
		lines = append(lines, rowBuilder.String())
	}
	return strings.Join(lines, "\n")
}

func truncateString(s string, maxLen int) string {
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
