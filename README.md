# xhs-mcp-patched

A reproducible Windows build of [xpzouying/xiaohongshu-mcp](https://github.com/xpzouying/xiaohongshu-mcp), based on upstream **v2.5.0**.

## Included fixes

### Detail timeout patch (v2.5.0-patched-detail.1)

- Pins upstream source to commit `6583124dfda92312b6bc19a042a6acfae63fe498`.
- Bounds detail work to 50 seconds after page acquisition, with a separate 15-second comment-scrolling budget.
- Saves note/comment snapshots before and during scrolling. Browser errors during comment loading return the saved snapshot with `comment_load_warning`; caller cancellation still returns an error.
- Uses error-returning navigation and extraction calls instead of panicking on those operations, and waits for note state rather than whole-page DOM stability.
- Tests timeout recovery, final snapshot retrieval, cancellation, and normal completion. CI builds a Windows executable.

This patch does not prove browser startup, login checks, tunnels, scheduled tasks, or Windows session persistence are fixed. The scrolling budget may return fewer comments than requested. A live Windows test is still required; keep the previous executable for rollback and keep the log-scanning watchdog disabled during validation.

### Existing search patch

- Wait for Xiaohongshu search data hydration instead of waiting for a continuously changing DOM to become stable.
- Limit search-page navigation to 12 seconds and retry once when the site stalls while returning HTML.

These changes address the repeated 60-second `search_feeds` timeouts seen on the Windows VPS deployment.

## Download

Use the latest release asset:

`xiaohongshu-mcp-windows-amd64-patched.exe`

The release also includes a SHA-256 checksum file. The executable is built by GitHub Actions from the pinned upstream tag plus the tracked patched source file, so the build is reproducible and auditable.

## Upstream and attribution

Original project: [xpzouying/xiaohongshu-mcp](https://github.com/xpzouying/xiaohongshu-mcp)

This repository only carries the patched search implementation and automated build recipe. Refer to the upstream repository for documentation and licensing.
