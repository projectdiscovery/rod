# ProjectDiscovery fork of go-rod

This repository is a fork of [go-rod/rod](https://github.com/go-rod/rod) v0.116.2, maintained by ProjectDiscovery for the headless browser needs of [nuclei](https://github.com/projectdiscovery/nuclei), [katana](https://github.com/projectdiscovery/katana), [httpx](https://github.com/projectdiscovery/httpx) and related tools. The module path is unchanged, so existing code moves to the fork with a `replace` directive and no import edits.

## Fork changes

Upstream go-rod is no longer released. These changes are the reason for the fork.

- **Local browsers talk over a pipe.** `Browser.Connect` launches Chrome with `--remote-debugging-pipe` on Linux and macOS, so no debugging port is open for other local processes to attach to. Windows falls back to a port, since Go does not pass extra file descriptors to child processes there. `Launcher.Launch` still returns a URL for callers that need one.
- **Connect cleans up after itself.** A cancelled context fails before a browser starts, and a browser that cannot finish connecting is closed rather than left running.
- **A page pool that checks its pages.** `NewHealthPagePool` hands out at most N pages. Before reuse, each page is checked: the browser answers, the target still exists, and the renderer runs script. A failed page is closed and its slot is freed, so a closed target is never handed out again. Returned pages go back to `about:blank` and popups they opened are closed. Cookies and storage are kept, so use a separate browser context when that state must not carry over.
- **Access to the paused network request.** `HijackRequest.Event()` returns the paused `Fetch.requestPaused` event, including the request id, the frame and the resource type.
- **Service worker requests.** A router attached to one page does not see requests that page's service worker makes itself, because those belong to the worker target. `Browser.HijackRequests` does see them. This is documented on `Page.HijackRequests` and covered by a test.
- **Chrome for Testing by default.** Without a `Bin`, the launcher downloads the latest Stable full Chrome, and the archive hash is pinned on first download. `HeadlessShell(true)` selects the smaller headless-shell build. Musl hosts use the installed browser instead of a download that will not run.
- **Browser exits with its parent.** Linux uses `Pdeathsig`, Windows a job object, and macOS and the BSDs a watchdog process. A crashed test run no longer leaves browsers behind.
- **Opt-in compliant mode.** `Compliant(true)` removes the automation switch, so `navigator.webdriver` is false, and makes the user agent and client hints match the real browser, without the `HeadlessChrome` token.
- **Examples and page tests run offline.** They use local fixture pages and a local HTTP server instead of live sites. Some launcher tests still download a browser and need network access.

## Upstream overview

[![Go Reference](https://pkg.go.dev/badge/github.com/go-rod/rod.svg)](https://pkg.go.dev/github.com/go-rod/rod)
[![Discord Chat](https://img.shields.io/discord/719933559456006165.svg)][discord room]

## [Documentation](https://go-rod.github.io/) | [API reference](https://pkg.go.dev/github.com/go-rod/rod?tab=doc) | [FAQ](https://go-rod.github.io/#/faq/README)

Rod is a high-level driver directly based on [DevTools Protocol](https://chromedevtools.github.io/devtools-protocol).
It's designed for web automation and scraping for both high-level and low-level use, senior developers can use the low-level packages and functions to easily
customize or build up their own version of Rod, the high-level functions are just examples to build a default version of Rod.

[中文 API 文档](https://pkg.go.dev/github.com/go-rod/go-rod-chinese)

## Features

- Chained context design, intuitive to timeout or cancel the long-running task
- Auto-wait elements to be ready
- Debugging friendly, auto input tracing, remote monitoring headless browser
- Thread-safe for all operations
- Automatically find or download [browser](lib/launcher)
- High-level helpers like WaitStable, WaitRequestIdle, HijackRequests, WaitDownload, etc
- Two-step WaitEvent design, never miss an event ([how it works](https://github.com/ysmood/goob))
- Correctly handles nested iframes or shadow DOMs
- No zombie browser process after the crash ([how it works](https://github.com/ysmood/leakless))
- [CI](https://github.com/go-rod/rod/actions) enforced 100% test coverage

## Examples

Please check the [examples_test.go](examples_test.go) file first, then check the [examples](lib/examples) folder.

For more detailed examples, please search the unit tests.
Such as the usage of method `HandleAuth`, you can search all the `*_test.go` files that contain `HandleAuth`,
for example, use GitHub online [search in repository](https://github.com/go-rod/rod/search?q=HandleAuth&unscoped_q=HandleAuth).
You can also search the GitHub [issues](https://github.com/go-rod/rod/issues) or [discussions](https://github.com/go-rod/rod/discussions),
a lot of usage examples are recorded there.

[Here is a comparison](lib/examples/compare-chromedp) of the examples between rod and Chromedp.

If you have questions, please raise an [issues](https://github.com/go-rod/rod/issues)/[discussions](https://github.com/go-rod/rod/discussions) or join the [chat room][discord room].

## Sponsors

Rod is sponsored by many organizations and individuals, thank you for your support!

Please contact [yad@ysmood.org](mailto:yad@ysmood.org) if you want to be listed here.

<!-- markdownlint-disable MD033 -->

<table style="border-collapse: collapse">
  <tr>
    <td>
      <p>Browser testing via</p>
      <a href="https://www.testmuai.com/?utm_medium=sponsor&utm_source=go-rod" target="_blank">
        <img
          src="https://github.com/user-attachments/assets/3efebe85-726b-49f1-b4c9-7c0103b192f3"
          alt="TestMu AI Logo"
          height="90"
        />
      </a>
    </td>
  </tr>
</table>

<!-- markdownlint-enable MD033 -->

## Join us

Your help is more than welcome! Even just open an issue to ask a question may greatly help others.

Please read [How To Ask Questions The Smart Way](http://www.catb.org/~esr/faqs/smart-questions.html) before you ask questions.

If you want to contribute please read the [Contributor Guide](.github/CONTRIBUTING.md).

[discord room]: https://discord.gg/CpevuvY
