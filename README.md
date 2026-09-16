# xhs-mcp-patched

A reproducible Windows build of [xpzouying/xiaohongshu-mcp](https://github.com/xpzouying/xiaohongshu-mcp), based on upstream **v2.5.0**.

## Included fixes

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
