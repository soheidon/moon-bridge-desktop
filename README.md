# Moon Bridge Desktop

Choose the right model and billing path for each task.

Moon Bridge Desktop is a Windows routing companion for Codex in the ChatGPT desktop app. Its goal is to let you decide, for each task or work plan, whether to use the Codex access included with your ChatGPT subscription or route the work to a separately billed model API such as DeepSeek—without changing your desktop workflow.

This makes it possible to reserve your ChatGPT subscription usage for tasks where you prefer OpenAI models, while using external provider APIs for other workloads.

> [!IMPORTANT]
> Moon Bridge Desktop v0.1.0 is a Technical Preview. The current production-supported provider scope is DeepSeek.

## Why Moon Bridge Desktop?

Different tasks do not always need the same model or billing source. Moon Bridge Desktop is being developed to provide a flexible way to:

* Use the Codex access included with a ChatGPT subscription
* Route selected workloads to an external model API
* Choose the appropriate reasoning profile for each task
* Keep using Codex in the ChatGPT desktop app
* Confirm which provider and routing profile handled each request
* Avoid permanently reconfiguring separate development environments

## Current Technical Preview

Version 0.1.0 implements the DeepSeek V4 routing path. It provides:

* A local Windows gateway for Codex in the ChatGPT desktop app
* Three named routing slots: Sol, Terra, and Luna
* Configurable reasoning behavior for each routing slot
* Request-level routing provenance
* Traffic Analysis and request correlation
* Safe autosave of routing observations

In v0.1.0, switching is still startup-based. Start or stop the Gateway, then restart the ChatGPT desktop app before changing between subscription-backed Codex access and DeepSeek API routing.

Seamless switching for each task or work plan is a product goal for a future version.

## Features

* Local gateway for routing OpenAI Responses API traffic from Codex in the ChatGPT desktop app to DeepSeek V4
* OpenAI Responses API-compatible endpoint
* Three named routing slots: Sol, Terra, and Luna
* Independently configurable provider, upstream model, operating mode, reasoning policy, and active routing profile for each slot
* Explicit routing-slot and profile provenance
* Configurable reasoning behavior for each routing slot
* Active-profile routing information preserved through provider dispatch and displayed in Traffic Analysis
* When a DeepSeek profile is configured with reasoning disabled, the request is explicitly prepared without DeepSeek thinking and remains disabled during processing
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
* Codex in the ChatGPT desktop app
* A DeepSeek API credential for separately billed DeepSeek API usage
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

1. Open the ChatGPT desktop app and select Codex.
2. Start Moon Bridge Desktop.
3. Configure the provider, upstream model, operating mode, and reasoning policy for each routing slot.
4. Select the active routing profile.
5. Start the Gateway.
6. Start or restart the ChatGPT desktop app after the Gateway is running.
7. Use Traffic Analysis when you need to inspect routing decisions and request correlation.

In v0.1.0, gateway selection should be treated as startup-time configuration. After starting or stopping the Gateway, restart the ChatGPT desktop app before changing between subscription-backed Codex access and DeepSeek API routing.

## Routing Slots

Moon Bridge Desktop provides three named routing slots: **Sol**, **Terra**, and **Luna**.

These names are stable aliases, not fixed model tiers. Each slot can be configured independently with:

* A provider
* An upstream model
* An operating mode
* A reasoning policy
* An active routing profile

For example, you may configure one slot for complex reasoning, another for everyday coding, and another for faster or lower-cost requests. These are possible configurations, not predefined meanings enforced by the slot names.

The active profile determines the actual provider, model, and reasoning behavior used for a request. Moon Bridge preserves this routing information through provider dispatch and displays it in Traffic Analysis.

When a DeepSeek profile is configured with reasoning disabled, Moon Bridge explicitly prepares the request without DeepSeek thinking and preserves that setting during request processing.

The v0.1.0 implementation uses the configured routing profile and slot assignments; it does not impose fixed Sol, Terra, or Luna reasoning tiers.

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
* Gateway switching is not yet seamless; restart the ChatGPT desktop app after changing the Gateway state.
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

* Simpler Gateway and ChatGPT desktop app Codex startup behavior
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
