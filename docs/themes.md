# zot themes

zot themes are JSON files that override any subset of the built-in
light/dark TUI theme. Nothing is required: a theme can change one
color, only the spinner, only syntax highlighting, or all of them.
Missing values inherit from zot's built-in default for the detected
terminal background.

## Where themes live

`$ZOT_HOME` below means zot's resolved state directory. The `ZOT_HOME`
environment variable is an optional override and may be unset or empty.
zot resolves the default internally without exporting it to your shell.
Before using `$ZOT_HOME` in shell commands,
[set it to your existing state directory](../README.md#credential-lookup-order).

User themes are discovered from:

```text
$ZOT_HOME/themes/*.json
```

Open `/settings` and choose **color theme** to switch. Changes are
saved in `$ZOT_HOME/config.json` and apply immediately. If the selected
file is deleted later, zot resets the setting to the built-in auto
(default) theme.

Theme files bundled with extensions are discovered in-place from loaded
extension directories:

```text
$ZOT_HOME/extensions/<extension>/theme.json
$ZOT_HOME/extensions/<extension>/themes/theme.json
<project>/.zot/extensions/<extension>/theme.json
<project>/.zot/extensions/<extension>/themes/theme.json
```

zot does **not** copy extension themes into `$ZOT_HOME/themes`; extension
owned themes stay in the extension directory. The settings picker shows
source info such as `from extension my-theme-extension`.

## WIP badge

The optional `wip` color key sets the active WIP status badge's ANSI-256 color.
If omitted, the badge uses the theme's `warning` color. For example:

```json
{"name":"draft","warning":214,"wip":208}
```

## Minimal themes

All of these are valid.

Metadata only:

```json
{
  "name": "my-theme",
  "description": "Metadata only; all visuals inherit zot defaults."
}
```

One shared color for both light and dark terminals:

```json
{
  "name": "pink-accent",
  "colors": {
    "accent": 204
  }
}
```

One color per mode:

```json
{
  "name": "split-accent",
  "colors": {
    "dark": { "accent": 204 },
    "light": { "accent": 161 }
  }
}
```

Spinner-only:

```json
{
  "name": "custom-spinner",
  "description": "Only changes the busy spinner.",
  "spinner_frames": ["◢", "◣", "◤", "◥"],
  "spinner_messages": ["working"],
  "spinner_interval_ms": 120
}
```

Dark-only themes still work on light terminals. If `colors.light` is
missing, zot applies `colors.dark` overrides on top of the built-in
light default. The inverse also works.

```json
{
  "name": "custom-spinner",
  "description": "An alternative spinner for zot that only displays a single spinner text.",
  "colors": {
    "dark": {
      "spinner_frames": ["◢", "◣", "◤", "◥"],
      "spinner_messages": ["working"],
      "spinner_interval_ms": 120
    }
  }
}
```

## Full shape

All fields are optional.

```json
{
  "name": "my-theme",
  "description": "Shown in /settings → color theme.",
  "color_descriptions": {
    "accent": "Optional documentation for humans. zot ignores this object."
  },
  "colors": {
    "dark": {
      "fg": 253,
      "muted": 244,
      "accent": 111,
      "background": "#0b1020",
      "user": 180,
      "user_bubble_bg": "#42454b",
      "user_bubble_fg": 248,
      "assistant": 117,
      "tool": 114,
      "tool_out": 245,
      "error": 203,
      "warning": 214,
      "spinner": 183,
      "selection_bg": 24,
      "selection_fg": 231,
      "spinner_frames": ["⠋", "⠙", "⠚", "⠞", "⠖", "⠦", "⠴", "⠲", "⠳", "⠓"],
      "spinner_messages": ["thinking", "working"],
      "spinner_interval_ms": 80,
      "syntax_base_style": "monokai",
      "syntax": {
        "keyword": "#81a1c1 bold",
        "keyword_constant": "#81a1c1",
        "keyword_declaration": "#81a1c1",
        "keyword_namespace": "#81a1c1",
        "keyword_reserved": "#81a1c1 bold",
        "keyword_type": "#88c0d0",
        "name_builtin": "#88c0d0",
        "name_function": "#8fbcbb",
        "name_class": "#a3be8c bold",
        "name_decorator": "#b48ead",
        "literal_string": "#a3be8c",
        "literal_string_escape": "#bf616a",
        "literal_number": "#d08770",
        "comment": "#616e88 italic",
        "comment_preproc": "#b48ead",
        "operator": "#eceff4",
        "punctuation": "#d8dee9",
        "text": "#e5e9f0"
      }
    },
    "light": {
      "fg": 236,
      "muted": 244,
      "accent": 33
    }
  }
}
```

You may also put overrides directly at the top level or directly under
`colors` when they should apply to both modes:

```json
{
  "name": "tiny",
  "accent": 204,
  "colors": {
    "spinner_messages": ["shipping"]
  }
}
```

## Color fields

Most color fields are xterm-256 indexes (`0`–`255`).

- `fg` — default foreground text.
- `muted` — secondary text, dividers, gutters, inactive hints.
- `accent` — prompt bar, bullets, links, headings, active markers.
- `background` — optional full-row TUI background. If missing, zot uses the terminal's existing background. Experimental: terminal background colors can vary by emulator and scrollback behavior; for the most reliable result, change your terminal background color in your terminal settings instead.
- `user` — user role label color; mostly compatibility.
- `user_bubble_bg` — background behind user message rows.
- `user_bubble_fg` — foreground inside user message rows.
- `assistant` — assistant/zot accent and spinner text.
- `tool` — tool names, success marks, diff additions.
- `tool_out` — plain tool-output text.
- `error` — errors, refused calls, diff deletions.
- `warning` — warnings and high context-usage state.
- `spinner` — reserved spinner color slot.
- `thinkingMax` or `thinking_max` (names retained for compatibility) - status color used when the opt-in `max` reasoning level is active. Missing values inherit the built-in theme color.
- `selection_bg` — highlighted row background.
- `selection_fg` — highlighted row foreground.

`background` and `user_bubble_bg` support richer terminal color forms:

```json
254
"#42454b"
{ "mode": "256", "index": 254 }
{ "mode": "ansi", "index": 100 }
{ "mode": "rgb", "r": 66, "g": 69, "b": 75 }
```

## Spinner fields

Spinner settings can appear at top level, under `colors`, or under
`colors.dark` / `colors.light`.

- `spinner_frames` — list of frame strings. Single-cell glyphs keep
  status-bar alignment clean.
- `spinner_messages` — list of messages; zot picks one per turn.
- `spinner_interval_ms` — frame interval in milliseconds. Missing or
  invalid falls back to 80ms.

## Syntax fields

Syntax highlighting uses Chroma style entries. Values may include
attributes after the color, such as `bold`, `italic`, or `underline`.

Supported syntax override keys:

```text
keyword, keyword_constant, keyword_declaration, keyword_namespace,
keyword_reserved, keyword_type, name_builtin, name_function,
name_class, name_decorator, literal_string, literal_string_escape,
literal_number, comment, comment_preproc, operator, punctuation, text
```

Example:

```json
{
  "colors": {
    "dark": {
      "syntax_base_style": "monokai",
      "syntax": {
        "keyword": "#f05b8d",
        "name_function": "#b675f1",
        "literal_string": "#58c760",
        "comment": "#a1a1a1 italic"
      }
    }
  }
}
```

## Theme-only extensions

An extension can exist only to ship a theme. No slash command,
subprocess, or executable is required when the extension contains a
valid theme file.

```text
$ZOT_HOME/extensions/my-theme-extension/
├── extension.json
└── theme.json
```

`extension.json`:

```json
{
  "name": "my-theme-extension",
  "version": "1.0.0",
  "description": "Ships a zot color theme",
  "enabled": true
}
```

No `exec` is needed when `theme.json` or `themes/theme.json` exists.
If `exec` is present, zot treats it as a normal extension too.

## Validate

zot theme files are plain JSON, not JSONC. Validate before installing:

```bash
python3 -m json.tool theme.json >/dev/null
```
