//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	configFileName     = "pipewire_matrix_config.json"
	virtualSinkName    = "pw_matrix_virtual_sink"
	virtualSinkDesc    = "PW-Matrix Hi-Res Master Hub"
	defaultSampleRate  = 48000
	defaultCaptureMs   = 8   // 8ms Hi-Res capture quantum (bebas jitter pipe)
	defaultRenderLatMs = 12  // 12ms Hi-Res hardware playback buffer
	captureChunkFrames = 256 // ~5.33ms @ 48kHz (power-of-two friendly)
	renderChunkFrames  = 256 // ~5.33ms @ 48kHz
	linuxF_SETPIPE_SZ  = 1031
)

// ============================================================================
// 0. PERSISTENT JSON CONFIGURATION SCHEMA
// ============================================================================

type EndpointSetting struct {
	Name          string  `json:"name"`
	Active        bool    `json:"active"`
	Volume        float32 `json:"volume"`
	Muted         bool    `json:"muted"`
	ExtraOffsetMs int32   `json:"extra_offset_ms"`
}

type PersistentConfig struct {
	TargetFloorMs uint32                     `json:"target_floor_ms"`
	AutoSyncMode  bool                       `json:"auto_sync_mode"`
	MasterVolume  float32                    `json:"master_volume"`
	Endpoints     map[string]EndpointSetting `json:"endpoints"`
}

func resolveConfigPath() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if !strings.Contains(strings.ToLower(dir), "temp") && !strings.Contains(strings.ToLower(dir), "go-build") && !strings.Contains(strings.ToLower(dir), "/tmp") {
			return filepath.Join(dir, configFileName)
		}
	}
	return configFileName
}

// ============================================================================
// 1. HI-RES LOCK-FREE SPSC RING BUFFER (ANTI-CRACKLE SMOOTH SERVO)
// ============================================================================

type SpscRingBuffer struct {
	buf      []float32
	capacity uint64
	writePos uint64 // atomic
	readPos  uint64 // atomic
	underrun uint64 // atomic
	overrun  uint64 // atomic

	// Single-consumer state (dimiliki eksklusif oleh renderTargetThread)
	currentGain   float32
	resamplePos   float64
	smoothedFill  float64
	smoothedRatio float64
	primed        bool
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
	w := atomic.LoadUint64(&r.writePos)
	atomic.StoreUint64(&r.readPos, w)
	r.resamplePos = 0
	r.smoothedFill = 0
	r.smoothedRatio = 0
	r.primed = false
}

func (r *SpscRingBuffer) WriteStereo(src []float32, frames uint64) uint64 {
	avail := r.AvailableWrite()
	if frames > avail {
		atomic.AddUint64(&r.overrun, 1)
		// Drop oldest frames cleanly to recover to half capacity
		drop := frames - avail + (r.capacity / 8)
		if drop > r.AvailableRead() {
			drop = r.AvailableRead()
		}
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

// ReadStereoAdaptive menggunakan Low-Pass Filtered Drift Servo agar bebas kresek-kresek
// (tidak terpengaruh burst pipe OS) dan menjaga sinkronisasi presisi antar DAC.
func (r *SpscRingBuffer) ReadStereoAdaptive(
	dst []float32,
	outFrames uint64,
	targetDspGain float32,
	muted bool,
	srcSampleRate uint32,
	dstSampleRate uint32,
	targetLatencyFrames uint64,
) uint64 {
	if outFrames == 0 {
		return 0
	}
	if srcSampleRate == 0 {
		srcSampleRate = defaultSampleRate
	}
	if dstSampleRate == 0 {
		dstSampleRate = defaultSampleRate
	}

	avail := r.AvailableRead()
	baseRatio := float64(srcSampleRate) / float64(dstSampleRate)
	if r.smoothedRatio == 0 {
		r.smoothedRatio = baseRatio
	}

	// Pre-charge ("Priming") buffer sampai 80% targetLatencyFrames sebelum mulai berbunyi
	// agar tidak langsung underrun (kresek di awal) saat stream baru dinyalakan.
	if !r.primed {
		if avail < (targetLatencyFrames*4)/5 {
			clear(dst[:outFrames*2])
			return 0
		}
		r.primed = true
		r.smoothedFill = float64(avail)
		r.smoothedRatio = baseRatio
	}

	// Hard snap hanya jika terjadi stall ekstrem (> 85ms di atas target, misal baru bangun dari suspend)
	maxTolerableSlack := uint64(srcSampleRate) * 85 / 1000
	if avail > targetLatencyFrames+maxTolerableSlack {
		drop := avail - targetLatencyFrames
		atomic.AddUint64(&r.readPos, drop)
		avail = targetLatencyFrames
		r.smoothedFill = float64(avail)
	}

	// Jika benar-benar kehabisan sampel, masuk kembali ke mode pre-charge (priming)
	neededMin := uint64(math.Ceil(float64(outFrames)*baseRatio)) + 4
	if avail < neededMin {
		atomic.AddUint64(&r.underrun, 1)
		r.primed = false
		clear(dst[:outFrames*2])
		return 0
	}

	// 1. Exponential Moving Average (EMA) pada level isi buffer
	// Meredam jitter pembacaan OS Pipe agar pitch tidak bergoyang (anti-crackling).
	r.smoothedFill = r.smoothedFill*0.96 + float64(avail)*0.04

	// 2. Ultra-gentle proportional servo (maksimal ±0.18% atau 1800 ppm, tidak terdengar telinga)
	driftError := r.smoothedFill - float64(targetLatencyFrames)
	driftAdjust := clampFloat64(driftError*0.0000015, -0.0018, 0.0018)
	targetRatio := baseRatio * (1.0 + driftAdjust)

	// 3. Slew-rate limiter pada perubahan rasio resampling
	r.smoothedRatio = r.smoothedRatio*0.92 + targetRatio*0.08
	stepRatio := r.smoothedRatio

	// Pastikan tidak membaca melebihi sampel yang tersedia
	maxNeeded := uint64(math.Ceil(float64(outFrames)*stepRatio)) + 3
	if maxNeeded > avail {
		stepRatio = float64(avail-3) / float64(outFrames)
	}

	goalGain := targetDspGain
	if muted {
		goalGain = 0.0
	}

	rd := atomic.LoadUint64(&r.readPos)
	cap := r.capacity
	curGain := r.currentGain
	const smoothAlpha = 0.0015 // De-zippering volume halus (~14ms @ 48kHz)

	pos := r.resamplePos
	for i := uint64(0); i < outFrames; i++ {
		curGain += smoothAlpha * (goalGain - curGain)

		intOffset := uint64(pos)
		frac := float32(pos - float64(intOffset))

		// 4-Point 3rd-Order Hermite Cubic Interpolation
		idx0 := ((rd + intOffset + cap - 1) % cap) * 2
		idx1 := ((rd + intOffset) % cap) * 2
		idx2 := ((rd + intOffset + 1) % cap) * 2
		idx3 := ((rd + intOffset + 2) % cap) * 2

		sL := hermite4(frac, r.buf[idx0], r.buf[idx1], r.buf[idx2], r.buf[idx3])
		sR := hermite4(frac, r.buf[idx0+1], r.buf[idx1+1], r.buf[idx2+1], r.buf[idx3+1])

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

func hermite4(frac, y0, y1, y2, y3 float32) float32 {
	c0 := y1
	c1 := 0.5 * (y2 - y0)
	c2 := y0 - 2.5*y1 + 2.0*y2 - 0.5*y3
	c3 := 0.5*(y3-y0) + 1.5*(y1-y2)
	return ((c3*frac+c2)*frac+c1)*frac + c0
}

func splitHybridVolume(slider float32) (osScalar float32, dspBoost float32) {
	if slider <= 0.001 {
		return 0.0, 0.0
	}
	if slider <= 1.0 {
		return slider, 1.0
	}
	return 1.0, 1.0 + (slider-1.0)*2.0
}

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
	if x > -0.99 && x < 0.99 {
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
// 2. FAST FLOAT32LE STREAM HELPERS
// ============================================================================

func decodeFloat32LE(rawBytes []byte, frames int, dspGain float32, dstStereo []float32) {
	for f := 0; f < frames; f++ {
		off := f * 8
		lBits := binary.LittleEndian.Uint32(rawBytes[off : off+4])
		rBits := binary.LittleEndian.Uint32(rawBytes[off+4 : off+8])
		l := math.Float32frombits(lBits)
		r := math.Float32frombits(rBits)

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

func encodeFloat32LE(srcStereo []float32, frames int, dstBytes []byte) {
	srcBytes := unsafe.Slice((*byte)(unsafe.Pointer(&srcStereo[0])), frames*8)
	copy(dstBytes[:frames*8], srcBytes)
}

func configurePipeSize(pipe io.Closer, sizeBytes int) {
	if f, ok := pipe.(*os.File); ok {
		_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), linuxF_SETPIPE_SZ, uintptr(sizeBytes))
	}
}

// ============================================================================
// 3. PIPEWIRE VIRTUAL MASTER SINK + HARDWARE MULTI-OUTPUT ENGINE
// ============================================================================

func boostProAudioThread() func() {
	tid, _, _ := syscall.Syscall(syscall.SYS_GETTID, 0, 0, 0)
	if tid == 0 {
		return func() {}
	}
	origPrio, err := syscall.Getpriority(syscall.PRIO_PROCESS, int(tid))
	if err != nil {
		origPrio = 0
	}
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, int(tid), -15); err != nil {
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, int(tid), -5)
	}
	return func() {
		_ = syscall.Setpriority(syscall.PRIO_PROCESS, int(tid), origPrio)
	}
}

type DeviceInfo struct {
	ID         string
	Name       string
	IsDefault  bool
	SampleRate uint32
	Channels   uint32
	Bits       uint32
}

type TargetState struct {
	Device      DeviceInfo
	ActiveFlag  uint32 // atomic (0 or 1)
	MutedFlag   uint32 // atomic (0 or 1)
	VolumeBits  uint32 // atomic float32 slider [0.0 .. 1.5]
	LastUserSet int64  // atomic unix nano timestamp
	Ring        *SpscRingBuffer
	StopChan    chan struct{}
	LastErr     atomic.Value

	SampleRate    uint32 // atomic
	Channels      uint32 // atomic
	Bits          uint32 // atomic
	BufferUs      uint32 // atomic (microseconds)
	ExtraOffsetMs int32  // atomic manual delay offset in ms (persisted in JSON)
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
	Devices       []DeviceInfo // Hanya berisi Hardware Output Sinks (Virtual Sink disembunyikan dari daftar target)
	Targets       map[string]*TargetState
	CaptureStop   chan struct{}
	IsCapturing   bool
	MasterMute    uint32
	MasterVolBits uint32
	TargetFloorMs uint32 // default 16ms untuk stabilitas Hi-Res
	SrcSampleRate uint32
	AutoSyncMode  uint32

	virtualModuleID string
	previousDefSink string
	cleanupOnce     sync.Once

	volCmdChan chan endpointVolCmd

	configMu    sync.Mutex
	configPath  string
	savedConfig PersistentConfig

	telemetryMu sync.Mutex
	Telemetry   AudioTelemetry
}

func NewAudioEngine() *AudioEngine {
	eng := &AudioEngine{
		Targets:       make(map[string]*TargetState),
		TargetFloorMs: 16,
		SrcSampleRate: defaultSampleRate,
		AutoSyncMode:  1,
		volCmdChan:    make(chan endpointVolCmd, 64),
		configPath:    resolveConfigPath(),
		savedConfig: PersistentConfig{
			TargetFloorMs: 16,
			AutoSyncMode:  true,
			MasterVolume:  1.0,
			Endpoints:     make(map[string]EndpointSetting),
		},
	}
	eng.SetMasterVolumeInternal(1.0)
	eng.LoadConfigFromDisk()
	eng.SetupVirtualMasterSink()
	go eng.osVolumeSyncWorker()
	return eng
}

// SetupVirtualMasterSink membuat Virtual Sink "PW-Matrix Hi-Res Master Hub" (f32le 48kHz),
// menjadikannya Default Output di CachyOS, dan memindahkan semua stream aplikasi aktif ke sana.
func (e *AudioEngine) SetupVirtualMasterSink() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 1. Simpan default sink lama (pastikan bukan virtual sink sisa sesi sebelumnya)
	if out, err := exec.CommandContext(ctx, "pactl", "get-default-sink").Output(); err == nil {
		curDef := strings.TrimSpace(string(out))
		if curDef != "" && curDef != virtualSinkName {
			e.previousDefSink = curDef
		}
	}

	// 2. Bersihkan modul virtual sink lama jika masih menggantung dari crash sebelumnya
	unloadExistingVirtualSinks()

	// 3. Buat module-null-sink 32-bit float 48kHz stereo
	out, err := exec.CommandContext(
		ctx,
		"pactl",
		"load-module",
		"module-null-sink",
		"sink_name="+virtualSinkName,
		"rate=48000",
		"channels=2",
		"format=float32le",
		fmt.Sprintf("sink_properties=device.description=\"%s\"", virtualSinkDesc),
	).Output()
	if err == nil {
		e.virtualModuleID = strings.TrimSpace(string(out))
	}

	// 4. Set Virtual Sink ke 100% unmuted dan jadikan Default Sink OS
	_ = exec.CommandContext(ctx, "pactl", "suspend-sink", virtualSinkName, "0").Run()
	_ = exec.CommandContext(ctx, "pactl", "set-sink-mute", virtualSinkName, "0").Run()
	osScalar, _ := splitHybridVolume(e.GetMasterVolume())
	pct := int(math.Round(float64(osScalar) * 100.0))
	_ = exec.CommandContext(ctx, "pactl", "set-sink-volume", virtualSinkName, fmt.Sprintf("%d%%", pct)).Run()
	_ = exec.CommandContext(ctx, "pactl", "set-default-sink", virtualSinkName).Run()

	// 5. Pindahkan aplikasi yang sedang memutar audio (Spotify, Browser, dll.) ke Virtual Hub
	e.moveActiveStreamsToVirtualSink()
}

func unloadExistingVirtualSinks() {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "pactl", "list", "short", "modules").Output()
	if err != nil {
		return
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.Contains(line, "sink_name="+virtualSinkName) {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				_ = exec.CommandContext(ctx, "pactl", "unload-module", fields[0]).Run()
			}
		}
	}
}

type pactlSinkInputJSON struct {
	Index      int               `json:"index"`
	Properties map[string]string `json:"properties"`
}

func (e *AudioEngine) moveActiveStreamsToVirtualSink() {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "pactl", "-f", "json", "list", "sink-inputs").Output()
	if err != nil {
		return
	}
	var inputs []pactlSinkInputJSON
	if err := json.Unmarshal(out, &inputs); err != nil {
		return
	}
	for _, inp := range inputs {
		appName := strings.ToLower(inp.Properties["application.name"])
		nodeName := strings.ToLower(inp.Properties["node.name"])
		mediaName := strings.ToLower(inp.Properties["media.name"])
		// Jangan pernah memindahkan stream playback milik PW-Matrix sendiri!
		if strings.Contains(appName, "pacat") ||
			strings.Contains(appName, "pw-matrix") ||
			strings.Contains(nodeName, "pw_matrix") ||
			strings.Contains(mediaName, "pw_matrix") {
			continue
		}
		_ = exec.CommandContext(ctx, "pactl", "move-sink-input", strconv.Itoa(inp.Index), virtualSinkName).Run()
	}
}

// Cleanup menghapus Virtual Sink dan mengembalikan Default Sink ke hardware semula saat keluar
func (e *AudioEngine) Cleanup() {
	e.cleanupOnce.Do(func() {
		e.mu.Lock()
		if e.IsCapturing && e.CaptureStop != nil {
			close(e.CaptureStop)
			e.IsCapturing = false
		}
		for _, t := range e.Targets {
			if t.IsActive() && t.StopChan != nil {
				close(t.StopChan)
				atomic.StoreUint32(&t.ActiveFlag, 0)
			}
		}
		e.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()

		if e.previousDefSink != "" && e.previousDefSink != virtualSinkName {
			_ = exec.CommandContext(ctx, "pactl", "set-default-sink", e.previousDefSink).Run()
		}
		if e.virtualModuleID != "" {
			_ = exec.CommandContext(ctx, "pactl", "unload-module", e.virtualModuleID).Run()
		} else {
			unloadExistingVirtualSinks()
		}
	})
}

func (e *AudioEngine) LoadConfigFromDisk() {
	e.configMu.Lock()
	defer e.configMu.Unlock()

	data, err := os.ReadFile(e.configPath)
	if err != nil {
		return
	}

	var cfg PersistentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return
	}
	if cfg.Endpoints == nil {
		cfg.Endpoints = make(map[string]EndpointSetting)
	}

	if cfg.TargetFloorMs >= 6 && cfg.TargetFloorMs <= 100 {
		atomic.StoreUint32(&e.TargetFloorMs, cfg.TargetFloorMs)
	} else {
		cfg.TargetFloorMs = 16
	}

	if cfg.MasterVolume > 0.0 && cfg.MasterVolume <= 1.5 {
		e.SetMasterVolumeInternal(cfg.MasterVolume)
	}

	if cfg.AutoSyncMode {
		atomic.StoreUint32(&e.AutoSyncMode, 1)
	} else {
		atomic.StoreUint32(&e.AutoSyncMode, 0)
	}

	e.savedConfig = cfg
}

func (e *AudioEngine) SaveConfigAsync() {
	go func() {
		e.configMu.Lock()
		defer e.configMu.Unlock()

		e.mu.RLock()
		e.savedConfig.TargetFloorMs = atomic.LoadUint32(&e.TargetFloorMs)
		e.savedConfig.AutoSyncMode = atomic.LoadUint32(&e.AutoSyncMode) == 1
		e.savedConfig.MasterVolume = e.GetMasterVolume()

		if e.savedConfig.Endpoints == nil {
			e.savedConfig.Endpoints = make(map[string]EndpointSetting)
		}

		for id, t := range e.Targets {
			e.savedConfig.Endpoints[id] = EndpointSetting{
				Name:          t.Device.Name,
				Active:        t.IsActive(),
				Volume:        t.GetVolume(),
				Muted:         t.IsMuted(),
				ExtraOffsetMs: atomic.LoadInt32(&t.ExtraOffsetMs),
			}
		}

		snapshot, err := json.MarshalIndent(e.savedConfig, "", "  ")
		e.mu.RUnlock()
		if err != nil {
			return
		}

		tmpPath := e.configPath + ".tmp"
		if err := os.WriteFile(tmpPath, snapshot, 0644); err == nil {
			_ = os.Rename(tmpPath, e.configPath)
		}
	}()
}

func (e *AudioEngine) ComputeTargetRingFrames(target *TargetState, srcRate uint32) uint64 {
	if srcRate == 0 {
		srcRate = defaultSampleRate
	}
	floorMs := float64(atomic.LoadUint32(&e.TargetFloorMs))
	myHwMs := float64(atomic.LoadUint32(&target.BufferUs)) / 1000.0 * 0.5
	myExtraMs := float64(atomic.LoadInt32(&target.ExtraOffsetMs))

	desiredRingMs := floorMs + myExtraMs

	if atomic.LoadUint32(&e.AutoSyncMode) == 1 {
		var maxTotalMs float64
		e.mu.RLock()
		for _, t := range e.Targets {
			if t.IsActive() {
				tHwMs := float64(atomic.LoadUint32(&t.BufferUs)) / 1000.0 * 0.5
				tExtraMs := float64(atomic.LoadInt32(&t.ExtraOffsetMs))
				tMinTotal := floorMs + tHwMs + tExtraMs
				if tMinTotal > maxTotalMs {
					maxTotalMs = tMinTotal
				}
			}
		}
		e.mu.RUnlock()

		syncedRingMs := maxTotalMs - myHwMs
		if syncedRingMs > desiredRingMs {
			desiredRingMs = syncedRingMs
		}
	}

	if desiredRingMs < 8.0 {
		desiredRingMs = 8.0
	}
	if desiredRingMs > 1500.0 {
		desiredRingMs = 1500.0
	}

	return uint64(float64(srcRate) * desiredRingMs / 1000.0)
}

func (e *AudioEngine) SetMasterVolumeInternal(v float32) {
	if v < 0 {
		v = 0
	} else if v > 1.5 {
		v = 1.5
	}
	atomic.StoreUint32(&e.MasterVolBits, math.Float32bits(v))
}

func (e *AudioEngine) SetMasterVolume(v float32) {
	if v < 0 {
		v = 0
	} else if v > 1.5 {
		v = 1.5
	}
	e.SetMasterVolumeInternal(v)
	osScalar, _ := splitHybridVolume(v)
	e.queueOSVolumeUpdate(virtualSinkName, true, osScalar, false, false)
	e.SaveConfigAsync()
}

func (e *AudioEngine) GetMasterVolume() float32 {
	return math.Float32frombits(atomic.LoadUint32(&e.MasterVolBits))
}

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
	e.queueOSVolumeUpdate(virtualSinkName, false, 0, true, isMuted)
	e.SaveConfigAsync()
	return isMuted
}

func (e *AudioEngine) IsMasterMuted() bool {
	return atomic.LoadUint32(&e.MasterMute) == 1
}

func (e *AudioEngine) SetDeviceVolume(deviceID string, newVol float32) {
	if newVol < 0.0 {
		newVol = 0.0
	} else if newVol > 1.5 {
		newVol = 1.5
	}

	e.mu.RLock()
	t := e.Targets[deviceID]
	e.mu.RUnlock()

	if t != nil {
		t.SetVolume(newVol)
		atomic.StoreInt64(&t.LastUserSet, time.Now().UnixNano())
	}

	osScalar, _ := splitHybridVolume(newVol)
	e.queueOSVolumeUpdate(deviceID, true, osScalar, false, false)
	e.SaveConfigAsync()
}

func (e *AudioEngine) ToggleDeviceMute(deviceID string) bool {
	e.mu.RLock()
	t := e.Targets[deviceID]
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

	e.queueOSVolumeUpdate(deviceID, false, 0, true, muted)
	e.SaveConfigAsync()
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

// ============================================================================
// PIPEWIRE / PACTL ENUMERATION & VOLUME CONTROL
// ============================================================================

type pactlSinkJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	SampleSpec  string `json:"sample_specification"`
	Mute        bool   `json:"mute"`
	Volume      map[string]struct {
		ValuePercent string `json:"value_percent"`
	} `json:"volume"`
	Properties map[string]string `json:"properties"`
}

type polledEndpoint struct {
	info   DeviceInfo
	osVol  float32
	osMute bool
}

func queryPipeWireSinks(origDefSink string) ([]polledEndpoint, *polledEndpoint, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	out, err := exec.CommandContext(ctx, "pactl", "-f", "json", "list", "sinks").Output()
	if err != nil {
		return nil, nil, err
	}

	var rawSinks []pactlSinkJSON
	if err := json.Unmarshal(out, &rawSinks); err != nil {
		return nil, nil, err
	}

	var hwList []polledEndpoint
	var virtualEp *polledEndpoint

	for idx, s := range rawSinks {
		if s.Name == "" {
			continue
		}
		name := s.Description
		if name == "" {
			if nick := s.Properties["node.nick"]; nick != "" {
				name = nick
			} else if desc := s.Properties["device.description"]; desc != "" {
				name = desc
			} else {
				name = fmt.Sprintf("PipeWire Sink #%d (%s)", idx+1, s.Name)
			}
		}

		var sumPct float64
		var volCount int
		for _, v := range s.Volume {
			pctStr := strings.TrimSuffix(strings.TrimSpace(v.ValuePercent), "%")
			var pct float64
			if _, err := fmt.Sscanf(pctStr, "%f", &pct); err == nil {
				sumPct += pct
				volCount++
			}
		}
		osVol := float32(1.0)
		if volCount > 0 {
			osVol = float32((sumPct / float64(volCount)) / 100.0)
			if osVol < 0 {
				osVol = 0
			} else if osVol > 1.5 {
				osVol = 1.5
			}
		}

		sr, ch, bits := parseSampleSpec(s.SampleSpec)

		ep := polledEndpoint{
			info: DeviceInfo{
				ID:         s.Name,
				Name:       name,
				IsDefault:  s.Name == origDefSink,
				SampleRate: sr,
				Channels:   ch,
				Bits:       bits,
			},
			osVol:  osVol,
			osMute: s.Mute,
		}

		if s.Name == virtualSinkName {
			cp := ep
			virtualEp = &cp
			continue
		}
		hwList = append(hwList, ep)
	}
	return hwList, virtualEp, nil
}

func parseSampleSpec(spec string) (sr uint32, ch uint32, bits uint32) {
	sr, ch, bits = defaultSampleRate, 2, 32
	parts := strings.Fields(spec)
	for _, p := range parts {
		switch {
		case strings.HasSuffix(p, "Hz"):
			var parsedSr uint32
			if _, err := fmt.Sscanf(strings.TrimSuffix(p, "Hz"), "%d", &parsedSr); err == nil && parsedSr > 0 {
				sr = parsedSr
			}
		case strings.HasSuffix(p, "ch"):
			var parsedCh uint32
			if _, err := fmt.Sscanf(strings.TrimSuffix(p, "ch"), "%d", &parsedCh); err == nil && parsedCh > 0 {
				ch = parsedCh
			}
		case strings.Contains(p, "16"):
			bits = 16
		case strings.Contains(p, "24"):
			bits = 24
		case strings.Contains(p, "32"):
			bits = 32
		}
	}
	return sr, ch, bits
}

func (e *AudioEngine) osVolumeSyncWorker() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case cmd := <-e.volCmdChan:
			applyEndpointVolumeChange(cmd)

		case <-ticker.C:
			e.pollAllEndpointsFromOS()
		}
	}
}

func applyEndpointVolumeChange(cmd endpointVolCmd) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	if cmd.setVol {
		scalar := cmd.volScalar
		if scalar < 0.0 {
			scalar = 0.0
		} else if scalar > 1.0 {
			scalar = 1.0
		}
		pct := int(math.Round(float64(scalar) * 100.0))
		_ = exec.CommandContext(ctx, "pactl", "set-sink-volume", cmd.deviceID, fmt.Sprintf("%d%%", pct)).Run()
	}
	if cmd.setMute {
		muteArg := "0"
		if cmd.mute {
			muteArg = "1"
		}
		_ = exec.CommandContext(ctx, "pactl", "set-sink-mute", cmd.deviceID, muteArg).Run()
	}
}

func (e *AudioEngine) pollAllEndpointsFromOS() {
	sinks, virtEp, err := queryPipeWireSinks(e.previousDefSink)
	if err != nil {
		return
	}

	// Sinkronisasi volume Master jika user mengubah volume sistem via tombol keyboard/DE
	if virtEp != nil {
		curMaster := e.GetMasterVolume()
		if !(curMaster > 1.0 && virtEp.osVol >= 0.98) {
			e.SetMasterVolumeInternal(virtEp.osVol)
		}
		muteVal := uint32(0)
		if virtEp.osMute {
			muteVal = 1
		}
		atomic.StoreUint32(&e.MasterMute, muteVal)
	}

	sinkMap := make(map[string]polledEndpoint, len(sinks))
	for _, s := range sinks {
		sinkMap[s.info.ID] = s
	}

	e.mu.RLock()
	devices := make([]DeviceInfo, len(e.Devices))
	copy(devices, e.Devices)
	e.mu.RUnlock()

	now := time.Now().UnixNano()

	for _, d := range devices {
		polled, ok := sinkMap[d.ID]
		if !ok {
			continue
		}

		e.mu.RLock()
		t := e.Targets[d.ID]
		e.mu.RUnlock()
		if t == nil {
			continue
		}

		if now-atomic.LoadInt64(&t.LastUserSet) < int64(600*time.Millisecond) {
			continue
		}

		curSlider := t.GetVolume()
		if !(curSlider > 1.0 && polled.osVol >= 0.98) {
			t.SetVolume(polled.osVol)
		}

		muteVal := uint32(0)
		if polled.osMute {
			muteVal = 1
		}
		atomic.StoreUint32(&t.MutedFlag, muteVal)
	}
}

func (e *AudioEngine) RefreshDevices() error {
	list, _, err := queryPipeWireSinks(e.previousDefSink)
	if err != nil {
		return err
	}

	e.configMu.Lock()
	savedCfg := e.savedConfig
	e.configMu.Unlock()

	e.mu.Lock()
	devList := make([]DeviceInfo, len(list))
	var targetsToAutoStart []*TargetState
	anySavedActive := false

	for _, ep := range savedCfg.Endpoints {
		if ep.Active {
			anySavedActive = true
			break
		}
	}

	for idx, item := range list {
		devList[idx] = item.info
		ts, exists := e.Targets[item.info.ID]
		savedEp, hasSaved := savedCfg.Endpoints[item.info.ID]

		if !exists {
			ts = &TargetState{
				Device:     item.info,
				Ring:       NewSpscRingBuffer(96000),
				SampleRate: item.info.SampleRate,
				Channels:   item.info.Channels,
				Bits:       item.info.Bits,
				BufferUs:   defaultRenderLatMs * 1000,
			}
			if hasSaved {
				atomic.StoreInt32(&ts.ExtraOffsetMs, savedEp.ExtraOffsetMs)
				ts.SetVolume(savedEp.Volume)
				if savedEp.Muted {
					atomic.StoreUint32(&ts.MutedFlag, 1)
				}
				osScalar, _ := splitHybridVolume(savedEp.Volume)
				e.queueOSVolumeUpdate(item.info.ID, true, osScalar, true, savedEp.Muted)

				if savedEp.Active {
					targetsToAutoStart = append(targetsToAutoStart, ts)
				}
			} else {
				ts.SetVolume(item.osVol)
				if item.osMute {
					atomic.StoreUint32(&ts.MutedFlag, 1)
				}
				// Jika belum ada config tersimpan, otomatis nyalakan speaker/DAC default sebelumnya
				if !anySavedActive && item.info.IsDefault {
					targetsToAutoStart = append(targetsToAutoStart, ts)
				}
			}
			e.Targets[item.info.ID] = ts
		} else {
			ts.Device = item.info
			atomic.StoreUint32(&ts.SampleRate, item.info.SampleRate)
			atomic.StoreUint32(&ts.Channels, item.info.Channels)
			atomic.StoreUint32(&ts.Bits, item.info.Bits)
			if !(ts.GetVolume() > 1.0 && item.osVol >= 0.98) {
				ts.SetVolume(item.osVol)
			}
			if item.osMute {
				atomic.StoreUint32(&ts.MutedFlag, 1)
			} else {
				atomic.StoreUint32(&ts.MutedFlag, 0)
			}
		}
	}
	e.Devices = devList

	for _, ts := range targetsToAutoStart {
		if !ts.IsActive() {
			ts.Ring.Reset()
			ts.StopChan = make(chan struct{})
			atomic.StoreUint32(&ts.ActiveFlag, 1)
			go e.renderTargetThread(ts, ts.StopChan)
		}
	}
	e.mu.Unlock()

	return nil
}

func (e *AudioEngine) StartVirtualMasterCapture() {
	e.mu.Lock()
	if e.IsCapturing && e.CaptureStop != nil {
		close(e.CaptureStop)
	}
	stopCh := make(chan struct{})
	e.CaptureStop = stopCh
	e.IsCapturing = true
	e.mu.Unlock()

	go e.captureLoopbackThread(stopCh)
}

func (e *AudioEngine) ToggleTarget(deviceID string) {
	e.mu.Lock()
	t, ok := e.Targets[deviceID]
	if !ok {
		e.mu.Unlock()
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
	e.mu.Unlock()

	e.SaveConfigAsync()
}

// buildCaptureCommand menangkap monitor dari Virtual Master Sink dalam format Hi-Res 32-bit Float
func buildCaptureCommand(ctx context.Context, sinkID string, sampleRate uint32) *exec.Cmd {
	fragBytes := captureChunkFrames * 8
	if _, err := exec.LookPath("parec"); err == nil {
		return exec.CommandContext(
			ctx,
			"parec",
			"--raw",
			"--device="+sinkID+".monitor",
			"--format=float32le",
			fmt.Sprintf("--rate=%d", sampleRate),
			"--channels=2",
			fmt.Sprintf("--latency-msec=%d", defaultCaptureMs),
			fmt.Sprintf("--process-time-msec=%d", defaultCaptureMs/2),
		)
	}
	return exec.CommandContext(
		ctx,
		"pw-record",
		"--raw",
		"--target", sinkID,
		"--format", "f32",
		"--rate", fmt.Sprintf("%d", sampleRate),
		"--channels", "2",
		fmt.Sprintf("--latency=%d/%d", fragBytes/8, sampleRate),
		"--properties=stream.capture.sink=true,node.dont-reconnect=true,media.role=DSP",
		"-",
	)
}

// buildRenderCommand mengirim stream 32-bit Float langsung ke Hardware Sink tanpa lewat Virtual Sink
func buildRenderCommand(ctx context.Context, sinkID string, sampleRate uint32) *exec.Cmd {
	if _, err := exec.LookPath("pacat"); err == nil {
		return exec.CommandContext(
			ctx,
			"pacat",
			"--raw",
			"--playback",
			"--device="+sinkID,
			"--client-name=pw_matrix_router",
			"--stream-name=pw_matrix_out",
			"--format=float32le",
			fmt.Sprintf("--rate=%d", sampleRate),
			"--channels=2",
			fmt.Sprintf("--latency-msec=%d", defaultRenderLatMs),
			fmt.Sprintf("--process-time-msec=%d", defaultRenderLatMs/3),
		)
	}
	return exec.CommandContext(
		ctx,
		"pw-play",
		"--raw",
		"--target", sinkID,
		"--format", "f32",
		"--rate", fmt.Sprintf("%d", sampleRate),
		"--channels", "2",
		fmt.Sprintf("--latency=%dms", defaultRenderLatMs),
		"--properties=node.dont-reconnect=true,media.role=DSP,node.name=pw_matrix_out",
		"-",
	)
}

func wakeSink(sinkID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = exec.CommandContext(ctx, "pactl", "suspend-sink", sinkID, "0").Run()
}

func (e *AudioEngine) captureLoopbackThread(stopCh chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	revertNice := boostProAudioThread()
	defer revertNice()

	const sampleRate = uint32(defaultSampleRate)
	const channels = uint32(2)
	const bits = uint32(32)

	atomic.StoreUint32(&e.SrcSampleRate, sampleRate)
	periodMs := float64(captureChunkFrames) * 1000.0 / float64(sampleRate)

	wakeSink(virtualSinkName)

	for {
		select {
		case <-stopCh:
			return
		default:
		}

		ctx, cancel := context.WithCancel(context.Background())
		stopWatcher := make(chan struct{})
		go func() {
			select {
			case <-stopCh:
				cancel()
			case <-stopWatcher:
			}
		}()

		cmd := buildCaptureCommand(ctx, virtualSinkName, sampleRate)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			close(stopWatcher)
			cancel()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		configurePipeSize(stdout, 16384)

		if err := cmd.Start(); err != nil {
			close(stopWatcher)
			cancel()
			time.Sleep(100 * time.Millisecond)
			continue
		}

		const frameBytes = 8
		rawBuf := make([]byte, captureChunkFrames*frameBytes)
		stereoScratch := make([]float32, captureChunkFrames*2)
		reader := bufio.NewReaderSize(stdout, captureChunkFrames*frameBytes*4)

		for {
			select {
			case <-stopCh:
				close(stopWatcher)
				cancel()
				_ = cmd.Wait()
				return
			default:
			}

			n, err := io.ReadFull(reader, rawBuf)
			if err != nil {
				e.decayTelemetry(sampleRate, channels, bits, periodMs)
				break
			}

			frames := n / frameBytes
			if frames <= 0 {
				continue
			}

			_, masterDspBoost := splitHybridVolume(e.GetMasterVolume())
			if e.IsMasterMuted() {
				masterDspBoost = 0.0
			}

			decodeFloat32LE(rawBuf[:frames*frameBytes], frames, masterDspBoost, stereoScratch[:frames*2])
			e.computeTelemetry(stereoScratch[:frames*2], sampleRate, channels, bits, periodMs)

			e.mu.RLock()
			for _, target := range e.Targets {
				if target.IsActive() {
					target.Ring.WriteStereo(stereoScratch[:frames*2], uint64(frames))
				}
			}
			e.mu.RUnlock()
		}

		close(stopWatcher)
		cancel()
		_ = cmd.Wait()
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *AudioEngine) renderTargetThread(target *TargetState, stopCh chan struct{}) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	revertNice := boostProAudioThread()
	defer revertNice()

	sampleRate := target.Device.SampleRate
	if sampleRate == 0 {
		sampleRate = defaultSampleRate
	}
	channels := target.Device.Channels
	if channels == 0 {
		channels = 2
	}
	bits := target.Device.Bits
	if bits == 0 {
		bits = 32
	}

	atomic.StoreUint32(&target.SampleRate, sampleRate)
	atomic.StoreUint32(&target.Channels, channels)
	atomic.StoreUint32(&target.Bits, bits)
	atomic.StoreUint32(&target.BufferUs, defaultRenderLatMs*1000)

	wakeSink(target.Device.ID)

	osScalar, _ := splitHybridVolume(target.GetVolume())
	e.queueOSVolumeUpdate(target.Device.ID, true, osScalar, true, target.IsMuted())

	frames := renderChunkFrames
	readScratch := make([]float32, frames*2)
	outBytes := make([]byte, frames*8)

	for {
		select {
		case <-stopCh:
			return
		default:
		}

		ctx, cancel := context.WithCancel(context.Background())
		stopWatcher := make(chan struct{})
		go func() {
			select {
			case <-stopCh:
				cancel()
			case <-stopWatcher:
			}
		}()

		cmd := buildRenderCommand(ctx, target.Device.ID, sampleRate)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			close(stopWatcher)
			cancel()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// Gunakan pipe 8KB (setara ~4 chunk @ 256 frame) agar tidak starvasi maupun over-buffering
		configurePipeSize(stdin, 8192)

		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf

		if err := cmd.Start(); err != nil {
			target.LastErr.Store(err.Error())
			close(stopWatcher)
			cancel()
			time.Sleep(100 * time.Millisecond)
			continue
		}

		target.Ring.Reset()
		target.LastErr.Store("")

		for {
			select {
			case <-stopCh:
				_ = stdin.Close()
				close(stopWatcher)
				cancel()
				_ = cmd.Wait()
				return
			default:
			}

			_, dspBoost := splitHybridVolume(target.GetVolume())
			muted := target.IsMuted()
			srcRate := atomic.LoadUint32(&e.SrcSampleRate)
			targetLatencyFrames := e.ComputeTargetRingFrames(target, srcRate)

			got := target.Ring.ReadStereoAdaptive(
				readScratch,
				uint64(frames),
				dspBoost,
				muted,
				srcRate,
				sampleRate,
				targetLatencyFrames,
			)
			if got == 0 {
				clear(outBytes)
			} else {
				encodeFloat32LE(readScratch, frames, outBytes)
			}

			if _, err := stdin.Write(outBytes); err != nil {
				errMsg := strings.TrimSpace(stderrBuf.String())
				if errMsg != "" {
					target.LastErr.Store(errMsg)
				}
				break
			}
		}

		_ = stdin.Close()
		close(stopWatcher)
		cancel()
		_ = cmd.Wait()
		time.Sleep(80 * time.Millisecond)
	}
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
		sr = defaultSampleRate
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
	eng.StartVirtualMasterCapture()
	return model{
		engine:    eng,
		statusMsg: fmt.Sprintf("Virtual Hub [%s] Active • Press [Space] on any device to route audio!", virtualSinkDesc),
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
			m.engine.SaveConfigAsync()
			m.engine.Cleanup()
			return m, tea.Quit

		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}

		case "down", "j":
			if m.cursor < devCount-1 {
				m.cursor++
			}

		case " ", "enter":
			if devCount > 0 {
				m.engine.mu.RLock()
				dev := m.engine.Devices[m.cursor]
				m.engine.mu.RUnlock()

				m.engine.ToggleTarget(dev.ID)
				m.statusMsg = fmt.Sprintf("Toggled & Saved Hi-Res Output: %s", dev.Name)
			}

		case "m":
			if devCount > 0 {
				m.engine.mu.RLock()
				dev := m.engine.Devices[m.cursor]
				m.engine.mu.RUnlock()
				muted := m.engine.ToggleDeviceMute(dev.ID)
				if muted {
					m.statusMsg = fmt.Sprintf("Muted & Saved Endpoint: %s", dev.Name)
				} else {
					m.statusMsg = fmt.Sprintf("Unmuted & Saved Endpoint: %s", dev.Name)
				}
			}

		case "M":
			muted := m.engine.ToggleMasterMute()
			if muted {
				m.statusMsg = "VIRTUAL MASTER HUB MUTED"
			} else {
				m.statusMsg = "VIRTUAL MASTER HUB UNMUTED"
			}

		case "left", "h":
			m.adjustSelectedVolume(-0.02)

		case "right", "l":
			m.adjustSelectedVolume(+0.02)

		case "[":
			newVol := m.engine.GetMasterVolume() - 0.02
			m.engine.SetMasterVolume(newVol)
			osScalar, dspBoost := splitHybridVolume(m.engine.GetMasterVolume())
			m.statusMsg = fmt.Sprintf("Virtual Hub Master Vol -> %.0f%% | DSP Boost: %.2fx", osScalar*100, dspBoost)

		case "]":
			newVol := m.engine.GetMasterVolume() + 0.02
			m.engine.SetMasterVolume(newVol)
			osScalar, dspBoost := splitHybridVolume(m.engine.GetMasterVolume())
			m.statusMsg = fmt.Sprintf("Virtual Hub Master Vol -> %.0f%% | DSP Boost: %.2fx", osScalar*100, dspBoost)

		case "-", "_":
			cur := atomic.LoadUint32(&m.engine.TargetFloorMs)
			if cur > 6 {
				atomic.StoreUint32(&m.engine.TargetFloorMs, cur-2)
				m.engine.SaveConfigAsync()
				m.statusMsg = fmt.Sprintf("Target Ring Buffer Floor tightened to %d ms (Saved)", cur-2)
			}

		case "=", "+":
			cur := atomic.LoadUint32(&m.engine.TargetFloorMs)
			if cur < 60 {
				atomic.StoreUint32(&m.engine.TargetFloorMs, cur+2)
				m.engine.SaveConfigAsync()
				m.statusMsg = fmt.Sprintf("Target Ring Buffer Floor relaxed to %d ms (Saved)", cur+2)
			}

		case "a":
			cur := atomic.LoadUint32(&m.engine.AutoSyncMode)
			next := cur ^ 1
			atomic.StoreUint32(&m.engine.AutoSyncMode, next)
			m.engine.SaveConfigAsync()
			if next == 1 {
				m.statusMsg = "AUTO LATENCY SYNC: ON (All active hardware outputs locked in phase)"
			} else {
				m.statusMsg = "AUTO LATENCY SYNC: OFF (Independent latency per hardware output)"
			}

		case ",":
			if devCount > 0 {
				m.engine.mu.RLock()
				dev := m.engine.Devices[m.cursor]
				t := m.engine.Targets[dev.ID]
				m.engine.mu.RUnlock()
				if t != nil {
					newOff := atomic.AddInt32(&t.ExtraOffsetMs, -2)
					if newOff < 0 {
						atomic.StoreInt32(&t.ExtraOffsetMs, 0)
						newOff = 0
					}
					m.engine.SaveConfigAsync()
					m.statusMsg = fmt.Sprintf("[%s] Saved Delay Offset: +%dms", truncateString(dev.Name, 24), newOff)
				}
			}

		case ".":
			if devCount > 0 {
				m.engine.mu.RLock()
				dev := m.engine.Devices[m.cursor]
				t := m.engine.Targets[dev.ID]
				m.engine.mu.RUnlock()
				if t != nil {
					newOff := atomic.AddInt32(&t.ExtraOffsetMs, 2)
					if newOff > 1000 {
						atomic.StoreInt32(&t.ExtraOffsetMs, 1000)
						newOff = 1000
					}
					m.engine.SaveConfigAsync()
					m.statusMsg = fmt.Sprintf("[%s] Saved Delay Offset: +%dms", truncateString(dev.Name, 24), newOff)
				}
			}

		case "r":
			_ = m.engine.RefreshDevices()
			m.engine.moveActiveStreamsToVirtualSink()
			m.statusMsg = "Re-enumerated Hardware Sinks & Routed Active Apps to Virtual Hub"
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
		m.statusMsg = fmt.Sprintf("[%s] Saved Vol: %.0f%% | DSP Boost: %.2fx", truncateString(dev.Name, 24), osScalar*100, dspBoost)
	}
}

func (m model) View() string {
	termWidth := m.width
	if termWidth <= 0 {
		termWidth = 126
	}

	m.engine.telemetryMu.Lock()
	tel := m.engine.Telemetry
	m.engine.telemetryMu.Unlock()

	m.engine.mu.RLock()
	devices := m.engine.Devices
	masterVol := m.engine.GetMasterVolume()
	masterMuted := m.engine.IsMasterMuted()
	floorMs := atomic.LoadUint32(&m.engine.TargetFloorMs)
	autoSync := atomic.LoadUint32(&m.engine.AutoSyncMode) == 1
	srcSr := atomic.LoadUint32(&m.engine.SrcSampleRate)
	if srcSr == 0 {
		srcSr = defaultSampleRate
	}

	syncBadge := pillSubtle.Render(fmt.Sprintf("FLOOR: %dms", floorMs))
	if autoSync {
		syncBadge = pillProAudio.Render(fmt.Sprintf("SYNC LOCK • %dms", floorMs))
	}

	masterDb := gainToDbString(effectiveTotalGain(masterVol), masterMuted)
	topHeader := lipgloss.JoinHorizontal(
		lipgloss.Center,
		headerBrandStyle.Render("⚡ PW-MATRIX HI-RES HUB"),
		" ",
		pillSubtle.Render("SRC: "+virtualSinkDesc+" (48kHz/32f)"),
		" ",
		syncBadge,
		" ",
		func() string {
			if masterMuted {
				return pillMute.Render("HUB MUTED")
			}
			return pillWarn.Render(fmt.Sprintf("HUB VOL %3.0f%% (%s)", masterVol*100, masterDb))
		}(),
	)

	availW := termWidth - 4
	if availW < 92 {
		availW = 92
	}
	leftW := int(float64(availW) * 0.52)
	if leftW < 54 {
		leftW = 54
	}
	if leftW > 74 {
		leftW = 74
	}
	rightW := availW - leftW - 1
	innerLeftW := leftW - 4
	innerRightW := rightW - 4

	nameColW := innerLeftW - 35
	if nameColW < 10 {
		nameColW = 10
	}

	var leftLines []string
	colHdr := fmt.Sprintf("  %-5s %-*s %-7s %-6s %-11s", "STATE", nameColW, "HARDWARE OUTPUT", "LATENCY", "OFFSET", "OS+DSP VOL")
	leftLines = append(leftLines, cyanText.Render("HARDWARE OUTPUT MATRIX")+" "+dimText.Render(fmt.Sprintf("(%d devices • Virtual Hub -> Multi-DAC)", len(devices))))
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
		case t != nil && t.IsActive() && t.IsMuted():
			roleTag = magentaText.Render("[MUT]")
		case t != nil && t.IsActive():
			roleTag = greenText.Render("[OUT]")
		case t != nil && t.IsMuted():
			roleTag = magentaText.Render("[mut]")
		default:
			roleTag = dimText.Render("[OFF]")
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

		latStr := dimText.Render("  --   ")
		if t != nil && t.IsActive() {
			ringMs := float64(t.Ring.AvailableRead()) * 1000.0 / float64(srcSr)
			hwMs := float64(atomic.LoadUint32(&t.BufferUs)) / 1000.0
			totalMs := ringMs + hwMs*0.5
			if totalMs < 35.0 {
				latStr = greenText.Render(fmt.Sprintf("%5.1fms", totalMs))
			} else {
				latStr = amberText.Render(fmt.Sprintf("%5.1fms", totalMs))
			}
		}

		offStr := dimText.Render("  0ms ")
		if t != nil {
			offMs := atomic.LoadInt32(&t.ExtraOffsetMs)
			if offMs > 0 {
				offStr = amberText.Render(fmt.Sprintf("+%-3dms", offMs))
			} else if t.IsActive() {
				offStr = mutedText.Render(" +0ms ")
			}
		}

		vol := float32(1.0)
		muted := false
		if t != nil {
			vol = t.GetVolume()
			muted = t.IsMuted()
		}
		volStr := renderCompactGain(vol, muted, 6)

		rowLine := fmt.Sprintf("%s%s %s %s %s %s", curGlyph, roleTag, paddedName, latStr, offStr, volStr)
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
		extraOff := atomic.LoadInt32(&selectedTarget.ExtraOffsetMs)
		ur := atomic.LoadUint64(&selectedTarget.Ring.underrun)
		or := atomic.LoadUint64(&selectedTarget.Ring.overrun)
		ringMs := float64(selectedTarget.Ring.AvailableRead()) * 1000.0 / float64(srcSr)
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
			line1 := fmt.Sprintf(
				"%s %3.0f%%  %s %4.2fx (%s)  %s %5dHz/%2dbit",
				dimText.Render("PW-OS:"), osScalar*100,
				dimText.Render("DSP:"), dspBoost, amberText.Render(dbStr),
				dimText.Render("FMT:"), sr, bits,
			)
			line2 := fmt.Sprintf(
				"%s %5.1fms  %s %4.1fms  %s +%dms  %s %d/%d",
				dimText.Render("RING:"), ringMs,
				dimText.Render("DAC:"), hwMs,
				dimText.Render("OFFSET:"), extraOff,
				dimText.Render("U/O:"), ur, or,
			)
			if lastErr, ok := selectedTarget.LastErr.Load().(string); ok && lastErr != "" {
				line2 = magentaText.Render("ERR: " + truncateString(lastErr, innerLeftW-6))
			}
			leftLines = append(leftLines, line1, line2)
		} else {
			line1 := fmt.Sprintf(
				"%s %3.0f%%  %s %4.2fx (%s)  %s +%dms",
				dimText.Render("PW-OS:"), osScalar*100,
				dimText.Render("DSP:"), dspBoost, amberText.Render(dbStr),
				dimText.Render("OFFSET:"), extraOff,
			)
			line2 := dimText.Render("[Space/Enter] Enable Output • [,/.] +2ms Offset • [a] Sync Lock")
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
		cyanText.Render("VIRTUAL HUB DSP TELEMETRY & LOGARITHMIC dBFS METER"),
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
		"[↑/↓] Nav  [Space/Enter] Toggle Out  [←/→] Vol  [m] Mute  [,/.] +Offset  [a] Sync Lock  [r] Route Apps  [q] Quit",
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
	return fmt.Sprintf("%+6.1fdB", db)
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
	defer engine.Cleanup()

	// Tangkap SIGINT/SIGTERM agar Virtual Sink selalu dibersihkan meskipun terminal ditutup paksa
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigCh
		engine.Cleanup()
		os.Exit(0)
	}()

	p := tea.NewProgram(initialModel(engine), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		engine.Cleanup()
		fmt.Fprintf(os.Stderr, "Fatal error: %v\n", err)
		os.Exit(1)
	}
}
