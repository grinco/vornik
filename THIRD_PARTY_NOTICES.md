# Third-party notices

Vornik ships the following third-party components in its own tree (vendored,
not fetched at build time). Each keeps its original licence, reproduced
below as that licence requires.

Go module dependencies are not vendored. They are inventoried, with their
licences, in the CycloneDX SBOM attached to every release
(`vornik.cdx.json` on the GitHub release page), which is the authoritative
list for that layer.

## htmx 1.9.10 and the htmx SSE extension

Files: `internal/ui/static/htmx.min.js`, `internal/ui/static/htmx-ext-sse.js`
Source: https://github.com/bigskysoftware/htmx (tag `v1.9.10`)
Licence: BSD 2-Clause

```
Copyright (c) 2020, Big Sky Software
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

## Adding a vendored file

If you copy a third-party file into this tree, add a section here with the
file path, the upstream source and version, the licence name, and the
licence text. The SBOM does not see vendored files; this page is the only
record of them.
