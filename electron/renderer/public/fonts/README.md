# public/fonts

Self-hosted WOFF2 subsets that ship with the Phos asset bundle so the UI
never reaches out to a CDN at launch (brief §3 + §14).

## Files

| File | Source | Approx. size |
|------|--------|--------------|
| `MonaspaceKrypton-Regular.woff2` | otf-monaspace-nerd 3.4.0 (MonaspiceKr) | 25 KB |
| `MonaspaceKrypton-Medium.woff2` | otf-monaspace-nerd 3.4.0 (MonaspiceKr) | 25 KB |
| `JetBrainsMono-Regular.woff2` | system /usr/share/fonts/TTF | 31 KB |
| `JetBrainsMono-Medium.woff2` | system /usr/share/fonts/TTF | 32 KB |
| `Inter-Regular.woff2` | Blender 5.1 datafiles, variable font (100..900) | 62 KB |
| `LICENSES/` | Upstream license files | -- |

Total: ~180 KB on disk, ~75 KB on the wire (already brotli-friendly).

## Subset

Each face is subsetted to:
- Latin basic (`U+0020-007E`)
- Latin-1 supplement (`U+00A0-00FF`)
- General punctuation (`U+2000-206F`)
- Arrows (`U+2190-21FF`)
- Box drawings + geometric shapes (`U+2580-25FF`)
- Bowtie operator `⋈` (`U+22C8`) -- the `M⋈IRAI` wordmark glyph

OpenType features kept: `kern`, `liga`, `calt`, `tnum`, `zero`. Krypton
also keeps stylistic sets `ss01-ss04` (the variable-mono geometric forms
that distinguish Krypton from sibling Monaspace cuts).

## Regenerate

```
./scripts/vendor-fonts.sh
```

The script reads from system sources (Arch packages + system fonts).
It is idempotent and overwrites the WOFF2s in this directory.

## License

All three families ship under permissive open-source licenses
(JetBrains Mono and Inter under SIL OFL 1.1; Monaspace under Apache
2.0). License texts live in `LICENSES/`.
