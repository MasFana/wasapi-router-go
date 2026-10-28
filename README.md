<div align="center">

# ⚡ Go WASAPI Real-Time Audio Router & DSP Telemetry (`wasapi-router-go`)

**Ultra-low-latency Windows Core Audio loopback mirroring matrix, 4-point Hermite cubic ASRC, cross-device latency synchronizer, and 30fps TUI spectrum analyzer built in pure Go.**

[![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8?style=for-the-badge&logo=go&logoColor=white)](https://golang.org/)
[![Platform](https://img.shields.io/badge/Platform-Windows%2010%2F11-0078D6?style=for-the-badge&logo=windows&logoColor=white)](https://www.microsoft.com/windows)
[![Architecture](https://img.shields.io/badge/Audio-WASAPI%20%2B%20MMCSS-00FF9C?style=for-the-badge)](https://learn.microsoft.com/en-us/windows/win32/coreaudio/wasapi)
[![DSP Engine](https://img.shields.io/badge/DSP-Hermite%20ASRC%20%2B%20Goertzel-00E5FF?style=for-the-badge)](#-mathematical--dsp-formulations)
[![License: MIT](https://img.shields.io/badge/License-MIT-FFB800?style=for-the-badge)](LICENSE)

*Simultaneously route, resample, phase-align, and analyze real-time Windows system audio across heterogeneous hardware endpoints (PCIe Soundcards, USB DACs, HDMI, and Bluetooth A2DP) down to the sub-10ms shared-mode hardware floor.*

[Overview](#-overview) • [UI Preview](#️-terminal-ui-preview) • [Key Features](#-key-architectural-features) • [Architecture](#-system-architecture) • [DSP Formulations](#-mathematical--dsp-formulations) • [Getting Started](#-getting-started) • [Research & Citation](#-academic-research--citation)

</div>

---

## 📖 Overview

**`wasapi-router-go`** is a zero-CGo, heap-safe **Windows Audio Session API (WASAPI) real-time loopback routing matrix and DSP telemetry suite** written in pure Go. Powered by [Bubble Tea](https://github.com/charmbracelet/bubbletea) and [Lip Gloss](https://github.com/charmbracelet/lipgloss), it captures system audio from any active Windows render endpoint and mirrors it simultaneously across multiple heterogeneous output devices (USB DACs, wired headphones, studio monitors, Bluetooth speakers, and virtual cables) down to the **~4–12ms shared-mode hardware floor**.

Broadcasting real-time audio across multiple independent soundcards introduces two classic systems engineering challenges: **asynchronous crystal clock drift** (causing buffer underruns/overruns) and **heterogeneous hardware buffer latencies** (causing acoustic echo/comb-filtering between speakers). Unlike standard audio routers that suffer from Go garbage-collection stutters, clock-drift pops, or digital attenuation loss, this engine implements:

1. **Windows MMCSS `Pro Audio` Real-Time Kernel Scheduling** (`THREAD_PRIORITY_TIME_CRITICAL`).
2. **Wait-Free / Lock-Free Atomic SPSC Ring Buffers** with zero heap allocations on hot audio threads.
3. **4-Point Hermite Cubic Fractional Resampling (ASRC)** governed by a proportional clock-drift controller.
4. **Automatic Cross-Device Latency Synchronization (`Sync Lock`)** & manual per-endpoint acoustic delay offsets.
5. **Bi-Directional Windows Native Volume Synchronization** paired with a $+6.0\text{ dB}$ de-zippered DSP preamp.

---

## 🖥️ Terminal UI Preview

![WASAPI RT-Matrix Terminal UI Preview](ui.png)

---

## ✨ Key Architectural Features

### 🏎️ Ultra-Low-Latency Audio Pipeline & ASRC
- **MMCSS `Pro Audio` Kernel Priority:** Pins capture and render goroutines to dedicated OS threads (`runtime.LockOSThread()`) and elevates them via `avrt.dll` (`AvSetMmThreadCharacteristicsW("Pro Audio")`) and `THREAD_PRIORITY_TIME_CRITICAL` (`15`) to prevent Windows background preemption or Go GC jitter.
- **Sub-10ms Period Negotiation:** Requests `AUDCLNT_STREAMFLAGS_LOWLATENCY` (`0x08000000`) with minimum hardware device periods (`minPeriod`), cutting shared-mode engine ticks down to `3ms–5ms` where supported by the audio driver.
- **4-Point Hermite Cubic ASRC & Clock-Drift Controller:** Bridges mismatched sample rates (e.g., $48\text{ kHz} \leftrightarrow 44.1\text{ kHz} \leftrightarrow 96\text{ kHz}$) on the fly using **3rd-order Hermite (Catmull-Rom) cubic interpolation**, preserving high-frequency response up to $20\text{ kHz}$. A proportional feedback controller dynamically adjusts fractional step ratios ($\pm 2.5\%$) to keep each target's ring buffer locked to the desired latency floor (`4ms–50ms`).
- **Bluetooth A2DP Hard-Snap Recovery:** Automatically flushes startup ring-buffer backlogs and hard-snaps read pointers whenever buffer bloat exceeds `45ms` above target latency, eliminating multi-second lag after wireless transport stalls.

### 🔗 Multi-Device Acoustic Synchronization & Persistence
- **Automatic Latency Sync Lock (`[a]`):** Continuously inspects hardware buffer sizes (`IAudioClient::GetBufferSize`) across all active outputs and dynamically delays faster DACs in their ring buffers so total acoustic latency matches the slowest active endpoint.
- **Per-Endpoint Manual Delay Offset (`[,]` / `[.]`):** Fine-tune acoustic phase alignment across physical speaker distances from `0ms` to `+1000ms` in `5ms` increments.
- **Atomic JSON State Persistence:** Automatically saves active routing topology, source selection, hybrid volume levels, mute states, and delay offsets to `wasapi_matrix_config.json` via atomic temporary-file swap.

### 🔊 Hybrid 2-Way Windows OS Volume & DSP Preamp
- **Bi-Directional Native Volume Sync (`IAudioEndpointVolume`):**
  - **0% – 100% Range:** Directly controls and stays synchronized with the **Windows Native Hardware Volume & Mute** state. Adjusting volume in the TUI moves the Windows 10/11 system tray slider in real time, and physical media wheel changes sync back into the TUI within `150ms`.
  - **Bit-Transparent Unity Gain:** While operating between `0%–100%`, internal DSP gain remains locked at `1.0x` (`0.0 dB`), avoiding double-attenuation and preserving full 24/32-bit dynamic range.
  - **100% – 150% Overdrive Preamp:** Pushing a slider past `100%` locks Windows hardware volume at `100%` and seamlessly engages a floating-point digital preamp boost up to `+6.0 dB` (`2.0x`).
- **Exponential De-Zippering & Padé $\tanh$ Soft-Limiter:** Applies a per-sample one-pole IIR smoothing filter ($\alpha = 0.0025$, $\tau \approx 8\text{ ms}$) on gain transitions to eliminate zipper noise, paired with a rational Padé approximant $\tanh(x)$ soft-limiter above $\pm 0.99$ ($-0.08\text{ dBFS}$) to prevent harsh digital clipping while keeping normal audio bit-transparent.

### 🛡️ Memory-Safe Multi-Format PCM Engine
- **Zero-Allocation Hot Loop:** Lock-free atomic SPSC ring buffers (`SpscRingBuffer`) and pre-allocated scratch slices (`16,384` stereo frames) ensure zero heap allocations on real-time audio threads.
- **Heap-Safe BlockAlign Codec:** Prevents Windows `0xc0000374` (`STATUS_HEAP_CORRUPTION`) crashes by dynamically parsing `nBlockAlign` and `wBitsPerSample` across **16-bit Signed Integer**, **24-bit Packed Integer**, and **32-bit IEEE Float** streams, with automatic mid-channel upmixing for 5.1/7.1 surround endpoints.
- **COM Memory Hygiene:** Strictly isolates `CoInitializeEx(0, COINIT_MULTITHREADED)` states per OS thread and frees `WAVEFORMATEX` structures via `CoTaskMemFree`.

### 📊 Real-Time DSP Telemetry Suite
- **Logarithmic dBFS VU Meters:** Dual-channel RMS + Peak bar graphs (`-60 dBFS` to `+3 dBFS`) with decaying peak-hold markers.
- **Stereo Phase & Panning Scope:** Real-time L/R energy balance meter and Pearson stereo phase correlation indicator (`-1.0` out-of-phase to `+1.0` mono).
- **16-Band Hann-Goertzel RTA Spectrum:** Logarithmic frequency bands (`45 Hz` – `13 kHz`) computed via windowed 2nd-order Goertzel IIR resonators with sub-character Unicode block resolution (`▁▂▃▄▅▆▇█`) and gravity-decay peak hold caps.

---

## 📐 System Architecture

```text
                        ┌───────────────────────────────────────────────┐
                        │     Windows OS Volume (IAudioEndpointVolume)  │
                        │     ◄─── 2-Way Sync (150ms Poll / Cmd) ───►   │
                        └───────────────────────┬───────────────────────┘
                                                │
┌───────────────────────┐   Event Callback      ▼
│  Source Audio Device  │ ──(3ms-10ms)──► ┌───────────────────────────┐
│  (WASAPI Loopback)    │   Raw PCM       │   decodeToStereoFloat()   │
└───────────────────────┘   16/24/32-bit  │ + Padé tanh Soft-Limiter  │
                                          └─────────────┬─────────────┘
                                                        │ Normalized Stereo Float32
                        ┌───────────────────────────────┼───────────────────────────────┐
                        ▼                               ▼                               ▼
          ┌───────────────────────────┐   ┌───────────────────────────┐   ┌───────────────────────────┐
          │  Target #1 SpscRingBuffer │   │  Target #2 SpscRingBuffer │   │     DSP Telemetry Tap     │
          │   (Lock-Free Atomic SPSC) │   │   (Lock-Free Atomic SPSC) │   │ • RMS / Peak Hold (dBFS)  │
          └─────────────┬─────────────┘   └─────────────┬─────────────┘   │ • L/R Phase Correlation   │
                        │                               │                 │ • 16-Band Hann-Goertzel   │
                        ▼                               ▼                 └─────────────┬─────────────┘
          ┌───────────────────────────┐   ┌───────────────────────────┐                 │
          │   ReadStereoAdaptive()    │   │   ReadStereoAdaptive()    │                 ▼
          │ • 4-Pt Hermite Cubic ASRC │   │ • 4-Pt Hermite Cubic ASRC │   ┌───────────────────────────┐
          │ • Prop. Drift Controller  │   │ • Prop. Drift Controller  │   │   Bubble Tea 30fps TUI    │
          │ • Sync Lock + Offset Delay│   │ • Sync Lock + Offset Delay│   │ • Routing Matrix + Scopes │
          │ • One-Pole Vol De-Zipper  │   │ • One-Pole Vol De-Zipper  │   │ • Atomic JSON Persistence │
          └─────────────┬─────────────┘   └─────────────┬─────────────┘   └───────────────────────────┘
                        │                               │
                        ▼                               ▼
          ┌───────────────────────────┐   ┌───────────────────────────┐
          │  Target Endpoint #1 DAC   │   │  Target Endpoint #2 DAC   │
          └───────────────────────────┘   └───────────────────────────┘
```

---

## 🧮 Mathematical & DSP Formulations

For academic reproducibility and systems engineering reference, the core signal processing algorithms implemented in `main.go` are defined below.

### 1. Proportional Clock-Drift Controller & ASRC Step Ratio
Given capture sample rate $F_s^{\text{src}}$ and target render sample rate $F_s^{\text{dst}}$, let $N_{\text{avail}}$ be the current readable frames in the SPSC ring buffer and $N_{\text{target}}$ be the desired target buffer fill. The fractional step ratio $\Delta t$ is modulated per callback block by a bounded proportional controller ($K_p = 2 \times 10^{-5}$):

$$R_{\text{base}} = \frac{F_s^{\text{src}}}{F_s^{\text{dst}}}, \quad e[k] = N_{\text{avail}}[k] - N_{\text{target}}$$

$$\Delta t[k] = R_{\text{base}} \cdot \left( 1 + \text{clip}\left( K_p \cdot e[k], \, -0.025, \, +0.025 \right) \right)$$

### 2. 4-Point, 3rd-Order Hermite (Catmull-Rom) Cubic Interpolation
For integer frame index $n = \lfloor t \rfloor$ and fractional sub-sample phase $\mu = t - n \in [0, 1)$, each stereo channel is reconstructed from four adjacent ring-buffer samples $y[n-1], y[n], y[n+1], y[n+2]$ evaluated in Horner form:

$$\begin{aligned} c_0 &= y[n] \\ c_1 &= \frac{1}{2}\left(y[n+1] - y[n-1]\right) \\ c_2 &= y[n-1] - \frac{5}{2}y[n] + 2y[n+1] - \frac{1}{2}y[n+2] \\ c_3 &= \frac{1}{2}\left(y[n+2] - y[n-1]\right) + \frac{3}{2}\left(y[n] - y[n+1]\right) \\ \hat{y}(n + \mu) &= \left((c_3 \mu + c_2)\mu + c_1\right)\mu + c_0 \end{aligned}$$

### 3. Cross-Device Latency Synchronization (`AutoSyncMode`)
Across all active output endpoints $j \in \mathcal{T}$ with hardware buffer duration $B_j$ (in ms), manual user offset $O_j$ (in ms), and global latency floor $F_{\text{floor}}$, the target ring-buffer delay $L_i^{\text{ring}}$ (in ms) for endpoint $i$ is:

$$L_{\max} = \max_{j \in \mathcal{T}} \left( F_{\text{floor}} + 0.5 B_j + O_j \right), \quad L_i^{\text{ring}} = \max\left( F_{\text{floor}} + O_i, \, L_{\max} - 0.5 B_i \right)$$

### 4. One-Pole Exponential De-Zippering & Padé Rational Soft-Limiter
Instantaneous digital gain $G[n]$ tracks target gain $G_{\text{goal}}$ via a first-order low-pass IIR filter ($\alpha = 0.0025$), followed by a bit-transparent rational soft-clipper above $-0.08\text{ dBFS}$ ($\vert{}x\vert{} \ge 0.99$):

$$G[n] = G[n-1] + \alpha \left( G_{\text{goal}} - G[n-1] \right), \quad f(x) = \begin{cases} x & \text{if } \vert{}x\vert{} < 0.99 \\ \text{sgn}(x) & \text{if } \vert{}x\vert{} > 3.0 \\ \dfrac{x(27 + x^2)}{27 + 9x^2} & \text{otherwise} \end{cases}$$

### 5. Stereo Phase Correlation & 16-Band Hann-Goertzel Spectrum
Zero-lag stereo phase correlation $\rho \in [-1, +1]$ and spectral magnitudes $\vert{}X(\omega_b)\vert{}$ across 16 logarithmic frequency centers $f_b = 45 \cdot (1.46)^b\text{ Hz}$ ($b \in [0, 15]$, window length $M \le 256$) are computed as:

$$\rho = \frac{\sum_{n=0}^{N-1} L[n] R[n]}{\sqrt{\sum_{n=0}^{N-1} L^2[n] \cdot \sum_{n=0}^{N-1} R^2[n]}}, \quad s_b[n] = w_{\text{Hann}}[n]\left(\frac{L[n]+R[n]}{2}\right) + 2\cos(\omega_b)s_b[n-1] - s_b[n-2]$$

---

## 🚀 Getting Started

### Prerequisites

- **OS:** Windows 10 or Windows 11 (x64 / ARM64)
- **Go:** Go `1.21+` (uses built-in `clear()` and `unsafe.Slice`)

### Installation & Build

1. **Clone the repository:**
   ```bash
   git clone [https://github.com/masfana/wasapi-router-go.git](https://github.com/masfana/wasapi-router-go.git)
   cd wasapi-router-go
   ```

2. **Fetch dependencies:**
   ```bash
   go mod tidy
   ```

3. **Run directly:**
   ```bash
   go run main.go
   ```

4. **Build an optimized standalone binary (Recommended for lowest latency):**
   ```bash
   go build -ldflags="-s -w" -o wasapi-router.exe main.go
   .\wasapi-router.exe
   ```

---

## 🕹️ Keyboard Controls

| Keybinding | Action |
| :--- | :--- |
| **`↑` / `k`** , **`↓` / `j`** | Navigate up and down the audio endpoint routing matrix |
| **`Space`** | Toggle active audio mirroring (`[OUT]`) for the highlighted target endpoint (auto-saved) |
| **`Enter` / `s`** | Set the highlighted endpoint as the active **Loopback Capture Source (`[SRC]`)** with Echo Guard |
| **`←` / `h`** , **`→` / `l`** | Adjust selected device volume (`±2%`): **0–100% Native Windows OS Vol** / **100–150% DSP Boost** |
| **`m`** | Mute / Unmute the selected endpoint (syncs directly with Windows Native Mute) |
| **`[` / `]`** | Decrease / Increase **Capture Source Master Volume** (`±2%`) |
| **`M`** | Toggle **Capture Source Master Mute** (mutes OS endpoint + loopback feed) |
| **`,` / `.`** | Decrease / Increase per-device **Manual Delay Offset** (`0ms` to `+1000ms` in `5ms` steps) |
| **`a`** | Toggle **Auto Latency Sync Lock** (aligns all active outputs to the slowest endpoint's latency) |
| **`-` / `+`** | Tighten / Relax target **Ring Buffer Latency Floor** (`4ms` to `50ms` in `2ms` steps) |
| **`r`** | Re-enumerate active Windows WASAPI endpoints & resync OS volume states |
| **`q` / `Ctrl+C`** | Gracefully save JSON configuration, stop streams, revert MMCSS handles, and exit |

---

## ⚡ Low-Latency & Multi-Speaker Tuning Guide

1. **Ultra-Low-Latency Monitoring (`4ms–10ms`):**
   - Press **`a`** to disable `SYNC LOCK` if you want a wired headphone/USB DAC to run at its absolute lowest independent latency without waiting for slower Bluetooth speakers.
   - Use **`-` / `+`** to dial the **Ring Buffer Floor**: `10ms` is rock-solid across all hardware; `4ms–6ms` is ideal for rhythm gaming, live synth monitoring, or real-time voice routing on wired PCIe/USB interfaces.
2. **Eliminating Room Echo Across Multiple Speakers (`SYNC LOCK` + `OFFSET`):**
   - When playing through wired desk speakers and a Bluetooth speaker simultaneously in the same room, enable **`[a]` (`SYNC LOCK`)** to automatically match their buffer latencies.
   - If the Bluetooth codec introduces additional internal DSP transport delay not reported to Windows WASAPI, select the faster wired speaker and press **`.`** (`+5ms` steps) until both speakers sound phase-locked.
3. **Sample Rate Matching (Optional Optimization):**
   - While the built-in **4-Point Hermite Cubic ASRC** transparently handles mismatched sample rates (e.g., $48\text{ kHz} \to 44.1\text{ kHz}$), setting your endpoints to the same native rate (e.g., **24-bit/32-bit @ 48000 Hz** in `mmsys.cpl`) allows Windows Audio Engine to bypass internal kernel resampling filters for maximum CPU efficiency.

---

## 🔬 Academic Research & Citation

**`wasapi-router-go`** is architected as a clean-room reference implementation suitable for computer science and audio engineering research in:
- **Real-Time Operating Systems (RTOS) & Multimedia Scheduling:** Empirical evaluation of Windows MMCSS `Pro Audio` thread boosting and event-driven WASAPI periodicity in garbage-collected runtimes (Go).
- **Lock-Free Concurrent Data Structures:** Throughput and worst-case execution time (WCET) analysis of atomic Single-Producer Single-Consumer (SPSC) ring buffers under multi-consumer fan-out workloads.
- **Adaptive Digital Signal Processing:** Comparative $\text{THD+N}$ and phase-jitter analysis of proportional clock-drift feedback controllers with 3rd-order Hermite cubic fractional interpolation across asynchronous DAC clock domains.

### BibTeX Citation

If you reference this software, its DSP pipeline, or its concurrency architecture in an academic paper, thesis, or technical report, please cite:

```bibtex
@software{wasapi_router_go_2026,
  author       = {Masfana},
  title        = {{wasapi-router-go: Ultra-Low-Latency Windows Core Audio Loopback Mirroring Engine, Hermite Cubic ASRC, and Real-Time DSP Telemetry in Go}},
  year         = {2026},
  publisher    = {GitHub},
  journal      = {GitHub repository},
  howpublished = {\url{[https://github.com/masfana/wasapi-router-go](https://github.com/masfana/wasapi-router-go)}}
}
```

---

## 🛠️ Tech Stack

- **[Charm Bracelet Bubble Tea](https://github.com/charmbracelet/bubbletea) & [Lip Gloss](https://github.com/charmbracelet/lipgloss):** Functional Elm-architecture TUI & declarative terminal layout styling.
- **[moutend/go-wca](https://github.com/moutend/go-wca) & [go-ole](https://github.com/go-ole/go-ole):** Pure Go COM bindings for `IMMDeviceEnumerator`, `IAudioClient`, `IAudioCaptureClient`, `IAudioRenderClient`, and `IAudioEndpointVolume`.
- **Windows System Interop (`golang.org/x/sys/windows`):** Kernel event handles (`CreateEvent`, `WaitForSingleObject`) and lazy-loaded `avrt.dll` / `kernel32.dll` syscalls for MMCSS `Pro Audio` real-time thread scheduling.

---

## 🛡️ License

Distributed under the **MIT License**. See [`LICENSE`](LICENSE) for full details.
