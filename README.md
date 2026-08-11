# Moon Bridge Desktop

Moon Bridge Desktop is a Windows desktop gateway that routes OpenAI Responses API traffic from Codex to DeepSeek V4.

It provides three routing profiles—Sol, Terra, and Luna—together with request-level observability, local Traffic Analysis, and a desktop interface for managing the gateway.

> [!IMPORTANT]
> Moon Bridge Desktop v0.1.0 is a Technical Preview. The current production-supported provider scope is DeepSeek.

## Features

* Local gateway for routing Codex requests to DeepSeek V4
* OpenAI Responses API-compatible endpoint
* Three routing profiles:

  * **Sol** — maximum reasoning
  * **Terra** — high reasoning
  * **Luna** — thinking disabled
* Explicit routing-slot and profile provenance
* Request correlation using safe local aliases
* Traffic Analysis for resolved routing and prepared provider requests
* Optional autosave of safe routing observations
* Protection against recording API keys, authorization headers, prompts, or raw request bodies
* Multilingual Windows installer:

  * English
  * Japanese
  * Simplified Chinese

## Requirements

* Windows 10 or Windows 11, x64
* Codex
* A DeepSeek API credential
* Microsoft WebView2 Runtime

If WebView2 Runtime is not already installed, the installer may require an internet connection to download it.

## Installation

1. Download the latest Windows installer from [GitHub Releases](https://github.com/soheidon/moon-bridge-desktop/releases).
2. Optionally verify the installer using the included `SHA256SUMS.txt`.
3. Run `Moon-Bridge-Desktop-v0.1.0-Windows-x64-Setup.exe`.
4. Select the installer language and complete the installation.

The v0.1.0 installer is not digitally signed. Windows may display an **Unknown publisher** or Microsoft Defender SmartScreen warning.

Install and uninstall smoke testing was not completed before the v0.1.0 Technical Preview release.

## Basic Usage

1. Start Moon Bridge Desktop.
2. Configure your DeepSeek API credential.
3. Configure the Sol, Terra, and Luna routing profiles.
4. Start the Gateway.
5. Start Codex after the Gateway is running.
6. Use Traffic Analysis when you need to inspect routing decisions and request correlation.

In v0.1.0, gateway selection should be treated as startup-time configuration. After starting or stopping the Gateway, restart Codex before changing between Moon Bridge routing and the original OpenAI connection.

## Routing Profiles

| Profile   | Reasoning behavior | Intended use                                            |
| --------- | ------------------ | ------------------------------------------------------- |
| **Sol**   | Maximum reasoning  | Complex tasks requiring the strongest reasoning setting |
| **Terra** | High reasoning     | General coding and reasoning work                       |
| **Luna**  | Thinking disabled  | Faster requests without DeepSeek thinking               |

Luna requests are explicitly prepared with thinking disabled and remain disabled after DeepSeek request processing.

## Traffic Analysis

Traffic Analysis records safe routing observations such as:

* Routing resolution
* Selected slot and profile
* Provider request preparation
* Request aliases used for correlation
* Safe gateway lifecycle information

API keys, authorization headers, prompts, raw request bodies, and correlation headers are not intended to be written to autosave logs.

## Current Limitations

* DeepSeek is the current production-supported provider.
* Google tool-use is not supported.
* MiniMax, Kimi, MiMo, and OpenRouter entries are placeholders and are not production-ready.
* Multiple-plugin E2E behavior remains unresolved.
* The Windows race-test environment currently fails to start with `0xC0000139`.
* Gateway switching is not yet seamless; Codex should be restarted after changing the Gateway state.
* The Windows installer is unsigned.
* Install and uninstall smoke testing was not performed for v0.1.0.

See [Known Issues](docs/known-issues.md) for the maintained list.

## Verification

The v0.1.0 release includes:

* Go tests, build, and vet
* Desktop web tests and production build
* Web UI tests and production build
* Loopback routing smoke tests
* Production-equivalent Sol, Terra, and Luna smoke tests
* Force-mock E2E validation without real provider credentials
* Static inspection of the Windows installer and executable metadata

The accepted E2E limitations are documented in [Known Issues](docs/known-issues.md).

## Development Status

Moon Bridge Desktop is under active development.

The next development phase will focus on:

* Simpler Gateway and Codex startup behavior
* More practical switching between Moon Bridge and the original OpenAI connection
* Installer validation and signing
* Improved credential management
* Expansion of supported providers after the DeepSeek path is stable

## Release

The current release is:

[Moon Bridge Desktop v0.1.0 — Technical Preview](https://github.com/soheidon/moon-bridge-desktop/releases/tag/v0.1.0)

## License

See [LICENSE](LICENSE) for license information.

## Disclaimer

Moon Bridge Desktop is an independent project and is not affiliated with or endorsed by OpenAI or DeepSeek.
