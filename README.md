# Moon Bridge Desktop

**Plan with Sol. Implement with DeepSeek. Make your ChatGPT Plus usage last longer.**

Moon Bridge Desktop is a Windows routing companion for Codex in the ChatGPT desktop app. Its goal is to help divide a development workflow between subscription-backed Codex and a separately billed external model API such as DeepSeek.

Moon Bridge Desktop does not increase ChatGPT usage limits or convert subscription usage into API credits. It lets you decide which billing path handles each phase of development.

External models may not behave identically to OpenAI models. The goal is to combine strong planning and review with a practical, separately billed implementation path.

> [!IMPORTANT]
> Moon Bridge Desktop v0.1.0 is a Technical Preview. The current production-supported provider scope is DeepSeek.

## Why Moon Bridge Desktop?

ChatGPT Plus includes access to Codex, but its usage limits can be restrictive during long or intensive development sessions.

Moon Bridge Desktop is designed to divide a development workflow between two billing paths:

1. **Plan with subscription-backed Codex**
   Use Sol through your ChatGPT subscription for architecture, difficult reasoning, task planning, and review.
2. **Implement with an external API**
   Start the Moon Bridge Gateway and route implementation work to a separately billed external model API such as DeepSeek.
3. **Preserve your ChatGPT Plus usage**
   Keep the limited subscription allowance available for the stages where you value Sol and OpenAI models most.

The goal is to make sustained software development practical even with a ChatGPT Plus plan: use subscription-backed Codex for high-value planning and use external API capacity for token-intensive implementation work, while continuing to work in the ChatGPT desktop app.

Moon Bridge Desktop does not increase ChatGPT usage limits or convert subscription usage into API credits. It lets you decide which billing path handles each phase of development.

External models may not behave identically to OpenAI models. The goal is to combine strong planning and review with a practical, separately billed implementation path.

## Sister Project: Anthro Bridge

If your development workflow also uses Claude Code Desktop, see [Anthro Bridge](https://github.com/soheidon/anthro-bridge).

While Claude Code CLI allows the use of third-party API providers and custom routing configurations, the Claude Desktop application does not permit this level of flexibility and is restricted to Anthropic's official backend. This creates a gap for developers who want to use Claude's interface while still routing requests through external model providers or alternative billing sources.

Anthro Bridge exists to solve this limitation.

Anthro Bridge takes a different approach from Moon Bridge Desktop, using a 3P Gateway-based architecture to route Anthropic-compatible workflows. It allows users to assign external model providers to configurable Opus, Sonnet, and Haiku routes and manage them through a desktop application.

Moon Bridge Desktop and Anthro Bridge are independently developed sister projects that share a common goal: giving developers more control over which models, providers, and billing sources handle different parts of their work.

* Moon Bridge Desktop focuses on Codex in the ChatGPT desktop app and OpenAI Responses API routing.
* Anthro Bridge focuses on Claude Code Desktop, Claude Cowork, and Anthropic-compatible routing.

Both projects are completely independent and can be used separately.

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

## Supported External Provider

Version 0.1.0 supports DeepSeek V4 as its external model API.

## Technical Preview Limitations

* Gateway changes currently require restarting the ChatGPT desktop app.
* The Windows installer is not digitally signed.
* Install and uninstall smoke testing was not completed before the v0.1.0 release.

## Verification

The v0.1.0 release includes:

* Go tests, build, and vet
* Desktop web tests and production build
* Web UI tests and production build
* Loopback routing smoke tests
* Production-equivalent Sol, Terra, and Luna smoke tests
* Force-mock E2E validation without real provider credentials
* Static inspection of the Windows installer and executable metadata

Internal provider and integration tests outside the supported DeepSeek scope are not part of the product support contract.

## Development Status

Moon Bridge Desktop is under active development.

The next development phase will focus on:

* Simpler Gateway and ChatGPT desktop app Codex startup behavior
* More practical switching between Moon Bridge and the original OpenAI connection
* Installer validation and signing
* Improved credential management
* Expansion of supported providers after the DeepSeek path is stable
* DeepSeek routing and Traffic Analysis improvements

## Release

The current release is:

[Moon Bridge Desktop v0.1.0 — Technical Preview](https://github.com/soheidon/moon-bridge-desktop/releases/tag/v0.1.0)

## License

See [LICENSE](LICENSE) for license information.

## Disclaimer

Moon Bridge Desktop is an independent project and is not affiliated with or endorsed by OpenAI or DeepSeek.
