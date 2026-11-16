# 💬 Neighbour Chat for KOReader

A secure, encrypted, LAN-based chat plugin designed specifically for e-ink devices running [KOReader](https://github.com/koreader/koreader). 

Neighbour Chat operates entirely over your local network. It allows devices to discover each other and communicate securely without relying on external servers, internet connectivity, or third-party accounts.

![Platform](https://img.shields.io/badge/platform-KOReader%20(Linux%2FAndroid)-lightgrey)
![Language](https://img.shields.io/badge/language-Lua%20%7C%20Go-blue)
![Status](https://img.shields.io/badge/status-Active-success)

---

## ✨ Features

* **🔒 Secure & Encrypted:** Utilizes native cryptography (`golang.org/x/crypto`) to ensure your local messages remain private.
* **📡 Serverless Discovery:** Automatically finds other KOReader devices running the plugin on your local area network (LAN). No manual IP configuration is required.
* **📖 E-Ink Optimized:** The UI is built using KOReader's native Lua widgets, ensuring a clean, ghosting-free, and responsive experience on e-reader displays.
* **⚡ Blazing Fast Backend:** Heavy lifting (networking, encryption, and SQLite database storage) is handled by a compiled Go binary, keeping the Lua frontend lightweight and fast.
* **🗄️ Embedded Vault:** Chats and data are stored locally in an embedded, encrypted SQLite database.

---

## 🚀 Installation

Because this plugin relies on a compiled native backend (written in Go) for your specific device architecture, **it is highly recommended to use the pre-compiled release.**

1. Go to the **[Releases](#)** page of this repository.
2. Download the latest `NeighbourChat-KOReader.zip` file.
3. Connect your e-reader to your computer via USB.
4. Extract the `.zip` file and copy the entire `neighbourchat.koplugin` folder into the `plugins/` directory of your KOReader installation.
   * *Example path:* `koreader/plugins/neighbourchat.koplugin/`
5. Safely eject your device and restart KOReader.

---

## 🖱️ How to Use

1. Open any document in KOReader, or navigate to the main File Browser.
2. Tap the top edge of the screen to bring up the top menu.
3. Go to the **Search** (🔍) or **Network** (🌐) tools menu.
4. Tap **Neighbour Chat** to open the chat panel.
5. The plugin will immediately begin scanning your local network for other active users. Tap a user to start chatting!

---

## 🧠 Under the Hood (For Developers)

Neighbour Chat uses a split architecture to bypass the limitations of running complex cryptographic and networking tasks directly in Lua on low-power devices.

### Architecture
* **Frontend (`*.lua`):** The user interface is driven by standard KOReader Lua scripts (`main.lua`, `panel.lua`, `presentation.lua`). It communicates with the backend via local IPC/sockets.
* **Backend (`native/`):** The core logic is a compiled Go binary (`bin/neighbour-chat`). It handles:
  * LAN peer discovery (`discovery.go`).
  * Remote socket connections (`remote.go`).
  * Security and Vault management (`security.go`, `vault.go`).
  * Data persistence using an embedded SQLite database (`sqlite_small.go`).

### Building from Source

If you wish to compile the backend yourself (e.g., for a different architecture), you will need Python 3 and a working Go environment.

1. Clone the repository.
2. Navigate to the `tools/` directory.
3. Run the build script: `python build-koreader.py`
4. The script will compile the Go binary and package the `.koplugin` folder for deployment.

---

## 📁 Project Structure

```text
NeighbourChat/
├── neighbourchat.koplugin/     # Ready-to-deploy plugin folder
│   ├── bin/
│   │   └── neighbour-chat      # Compiled Go backend
│   ├── LICENSES/               # Open-source license notices
│   ├── client.lua              # Lua-to-Go bridge
│   ├── main.lua                # Plugin entry point
│   ├── panel.lua               # Chat interface UI
│   └── _meta.lua               # KOReader plugin metadata
├── native/                     # Go Backend Source
│   ├── api.go                  # API routing
│   ├── app.go                  # Core application logic
│   ├── discovery.go            # Network discovery
│   ├── security.go             # Cryptography
│   └── store.go                # SQLite operations
└── tools/
    └── build-koreader.py       # Compilation script
```

---

## 📜 License

*(Note: Please update this section with your chosen open-source license, such as MIT or GPLv3).* 

This project utilizes several fantastic open-source libraries (Go, SQLite, Musl, Zig-runtime). Their respective licenses can be found in the `LICENSES/` directory.