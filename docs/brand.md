# Canopy brand assets

A small set of canonical typographic + ASCII assets so canopy's identity stays consistent across the README, `canopy --help`, future docs, and any launch material.

## The mark

```
  ^
 ___
```

A caret floating above an underscore — a stylized canopy peak above its base. Pure ASCII (renders identically in every terminal, every editor, every browser, every paste). Reads as a tiny architectural canopy / awning, on-concept for the project name without falling into pine-tree-Christmas territory.

### Sizes

**Tiny (inline, favicon scale, prompts, badges):**
```
^_
```

**Small / Medium (default header glyph):**
```
  ^
 ___
```

**Hero (matches the figlet wordmark width):**
```
    ^
  _____
```

### When to use which

- `^_` — anywhere a single-glyph mark fits inline. Tweet copy, terminal prompts, error-line prefixes, places where vertical space is one row.
- `^ / ___` — the README header glyph (above the figlet wordmark), `canopy --help` long description, anywhere with 2-3 vertical lines of breathing room.
- The hero size is for landing-page-style display only; not used in the repo today.

## The wordmark

The figlet "Big" font rendering of "canopy":

```
   _____
  / ____|
 | |     __ _ _ __   ___  _ __  _   _
 | |    / _` | '_ \ / _ \| '_ \| | | |
 | |___| (_| | | | | (_) | |_) | |_| |
  \_____\__,_|_| |_|\___/| .__/ \__, |
                         | |     __/ |
                         |_|    |___/
```

Used in: README header. Always paired with the mark above it.

## Tone

- **Calm, focused.** Anti-busy. The mark is one caret and one underscore. The wordmark is a single typeface. Nothing more.
- **Terminal-native.** The brand should look correct in a 80×24 TTY, not just a high-res README preview. ASCII before Unicode; Unicode only when it materially helps.
- **Anti-mascot.** No animals, no anthropomorphic identity, no "the canopy bird." See [`docs/landscape.md`](landscape.md): canopy's positioning is "calm beats busy."

## Color

Canopy has no committed brand color at v0.1.0. The TUI uses lipgloss's terminal palette (256-color), with status-coded accents (green=ready, amber=stopped, red=broken, etc.) — these are functional, not branded. A canonical brand color is a v0.5+ decision, paired with whatever landing page exists then.

## Usage notes

- **Do not** translate the mark into a non-ASCII glyph (no emoji ⛺ or 🌳 substitutions, no Unicode arrows). The plain ASCII `^_` is the mark.
- **Do not** add extra characters to the mark (no `^*_*`, no `>^_`). One caret, one underscore.
- **Do not** commission a designer-rendered version that drifts from this shape. If a future v0.5 logo is designed, it should derive from this caret+underscore relationship — peak above base — at a minimum.
- **Do** use the inline `^_` form anywhere a one-glyph mark adds personality. Examples: `^_ canopy v0.1.0 — workspaces under one branch`.
