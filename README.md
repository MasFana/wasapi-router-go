# GO WASAPI Low-Latency Audio Router & DSP Telemetry

[![Go Version](https://img.shields.io/badge/Go-1.21%2B-blue.svg)](https://golang.org/)
[![Platform](https://img.shields.io/badge/Platform-Windows-lightgrey.svg)](https://www.microsoft.com/windows)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A blazing-fast, heap-safe **Windows WASAPI loopback audio router and real-time DSP telemetry system** built entirely in Go. Designed with an interactive terminal user interface (TUI) powered by [BubbleTea](https://github.com/charmbracelet/bubbletea) and styled with [Lip Gloss](https://github.com/charmbracelet/lipgloss), this application allows you to capture system audio loopback streams and route them dynamically across multiple active audio render endpoints with minimal latency.

---

## ✨ Key Features

* **Low-Latency WASAPI Loopback & Render:** Direct integration with Windows Audio Session API (WASAPI) via `go-wca` for real-time audio capture and multi-target endpoint mirroring.
* **Lock-Free SPSC Ring Buffer:** Custom heap-safe single-producer single-consumer ring buffer handling stereo float32 streaming safely between audio threads without runtime allocations or garbage collection pauses.
* **Dynamic BlockAlign PCM Engine:** Safeguards against heap corruption (`0xc0000374`) by strictly decoding and encoding raw byte buffers matching Windows audio engine constraints.
* **Real-Time DSP Telemetry:** 
  * Peak and RMS level meters with dynamic dB calculation.
  * 16-band frequency spectrum analyzer (50Hz – 16kHz) utilizing Go-optimized Goertzel/bandpass math.
* **Interactive TUI Dashboard:** Built with Charm Bracelet's BubbleTea ecosystem, offering live volume controls, endpoint matrix selection, stream muting, and error tracking (underruns/overruns).

---

## 🛠️ Tech Stack

* **Language:** Go (Golang)
* **TUI Framework:** Charm Bracelet (`bubbletea`, `lipgloss`)
* **Audio Bindings:** `go-ole`, `go-wca`
* **System Interop:** `golang.org/x/sys/windows`

---

## 🚀 Getting Started

### Prerequisites

* **Operating System:** Windows 10 or Windows 11 (WASAPI loopback requires Windows).
* **Go Environment:** Go 1.21 or higher installed on your machine.

### Installation & Running

1. **Clone the repository:**
   ```bash
   git clone [https://github.com/masfana/wasapi-router-go.git](https://github.com/masfana/wasapi-router-go.git)
   cd wasapi-router-go
    ```

2. **Download dependencies:**
    ```bash
    go mod tidy
    ```


3. **Run the application:**
*(Note: The build tag `//go:build windows` ensures it compiles strictly for Windows audio subsystems).*
    ```bash
    go run main.go
    ```



---

## 🕹️ Keyboard Shortcuts

| Key | Action |
| --- | --- |
| **`↑` / `↓**` | Navigate through the audio endpoint matrix |
| **`Enter` / `s**` | Set the currently highlighted endpoint as the active loopback capture source |
| **`Space`** | Toggle target render stream mirroring for the selected endpoint |
| **`←` / `→**` | Adjust target volume (-5% / +5%) |
| **`m`** | Mute/Unmute the selected target render stream |
| **`[ / ]`** | Decrease / Increase Master Loopback Gain |
| **`M`** | Toggle Master Mute |
| **`r`** | Re-enumerate active Windows WASAPI render endpoints |
| **`q` / `Ctrl+C**` | Quit the application |

---

## 📐 Architecture Overview

```text
 ┌──────────────────────┐     Raw PCM Bytes     ┌────────────────────────┐
 │ Windows Audio Engine │ ───────────────────►  │  decodeToStereoFloat   │
 └──────────────────────┘                       └───────────┬────────────┘
                                                            │ Stereo Float32
                                                            ▼
 ┌──────────────────────┐                       ┌────────────────────────┐
 │Target Audio Endpoints│ ◄──────────────────── │   SpscRingBuffer       │
 └──────────────────────┘   encodeFromStereo    └───────────┬────────────┘
                                                            │ Telemetry Feed
                                                            ▼
                                                ┌────────────────────────┐
                                                │ BubbleTea TUI / DSP    │
                                                └────────────────────────┘

```

---

## 🛡️ License

Distributed under the MIT License. See `LICENSE` for more information.
