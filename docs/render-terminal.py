#!/usr/bin/env python3
"""Render a terminal transcript to a self-contained SVG for the README.

GitHub renders SVGs in an <img> sandbox, so no webfonts and no CSS
imports — everything here is inline attributes and a system monospace
stack. Columns line up because every glyph in the stack is monospace and
each line is one <text> run; we never rely on a specific font's advance
width being some exact number of pixels.

Usage:  python3 docs/render-terminal.py <transcript.txt> <out.svg> [title]

Transcript markup — one line each, prefix decides the colour:
  $ cmd       prompt line (green $, bright command, dim --flags)
  >  text     bright/emphasised
  .  text     dim
  #  text     comment (dimmest)
  |  text     plain body text; status codes are auto-coloured
  (blank)     spacer
"""
import html
import re
import sys

# Same palette as mygrokd's web pages, so docs read as one system.
BG = "#0d1119"
CHROME = "#11151f"
HAIRLINE = "#242c3d"
INK = "#e7eaf2"
DIM = "#8a93a6"
DIMMER = "#5b6478"
SIGNAL = "#b8f068"
WARN = "#ffb86b"
DANGER = "#ff8a8a"
CYAN = "#7fd7e8"

FONT = ("ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, "
        "'DejaVu Sans Mono', 'Liberation Mono', monospace")
FS = 14          # font size
LH = 22          # line height
CW = FS * 0.6    # nominal advance width, for sizing the canvas only
PAD_X = 24
PAD_TOP = 52     # room for the window chrome
PAD_BOT = 20


# A mygrok request log line: time, client IP, method, status phrase, then
# duration and path. Only the status phrase is coloured, matching what the
# real CLI prints.
LOG_RE = re.compile(
    r"^(?P<pre>\d\d:\d\d:\d\d\s+\S+\s+[A-Z]+\s+)"
    r"(?P<status>\d{3}(?:\s[A-Za-z-]+)*)"
    r"(?P<rest>.*)$"
)


def status_colour(code):
    """Same thresholds as statusColor() in cmd/mygrok/main.go."""
    if code >= 500:
        return DANGER
    if code >= 400:
        return WARN
    if code >= 300:
        return CYAN
    if code >= 100:
        return SIGNAL
    return DIM


def span(text, colour, weight=None):
    attrs = f'fill="{colour}"'
    if weight:
        attrs += f' font-weight="{weight}"'
    return f'<tspan {attrs}>{html.escape(text)}</tspan>'


def render_line(raw):
    """One transcript line → the tspans that make up its <text> body."""
    if not raw.strip():
        return ""
    kind, _, body = raw.partition(" ")

    if kind == "$":
        out = [span("$ ", SIGNAL, "bold")]
        # Command words bright, --flags dim: mirrors how the shell reads.
        for i, word in enumerate(body.split(" ")):
            if i:
                out.append(span(" ", INK))
            out.append(span(word, DIM if word.startswith("-") else INK,
                            "bold" if i == 0 else None))
        return "".join(out)
    if kind == ">":
        return span(body, INK, "bold")
    if kind == ".":
        return span(body, DIM)
    if kind == "#":
        return span(body, DIMMER)

    m = LOG_RE.match(body)
    if not m:
        return span(body, DIM)
    colour = status_colour(int(m["status"][:3]))
    return (span(m["pre"], DIM)
            + span(m["status"], colour)
            + span(m["rest"], DIM))


def render(lines, title):
    width = int(max(len(l) for l in lines) * CW) + PAD_X * 2 + 40
    width = max(width, 720)
    height = len(lines) * LH + PAD_TOP + PAD_BOT

    out = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" '
        f'height="{height}" viewBox="0 0 {width} {height}" '
        f'font-family="{FONT}" font-size="{FS}">',
        f'<rect width="{width}" height="{height}" rx="10" fill="{BG}"/>',
        f'<path d="M10 0h{width-20}a10 10 0 0 1 10 10v26H0V10A10 10 0 0 1 10 0z" '
        f'fill="{CHROME}"/>',
        f'<line x1="0" y1="36" x2="{width}" y2="36" stroke="{HAIRLINE}"/>',
    ]
    for i, colour in enumerate((DANGER, WARN, SIGNAL)):
        out.append(f'<circle cx="{22 + i * 18}" cy="18" r="5" fill="{colour}" '
                   f'opacity="0.75"/>')
    out.append(
        f'<text x="{width/2}" y="23" text-anchor="middle" fill="{DIMMER}" '
        f'font-size="12">{html.escape(title)}</text>')

    for i, line in enumerate(lines):
        body = render_line(line)
        if not body:
            continue
        y = PAD_TOP + i * LH
        out.append(f'<text x="{PAD_X}" y="{y}" xml:space="preserve">{body}</text>')

    out.append('</svg>')
    return "\n".join(out) + "\n"


def main():
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    src, dst = sys.argv[1], sys.argv[2]
    title = sys.argv[3] if len(sys.argv) > 3 else "mygrok"
    lines = open(src).read().rstrip("\n").split("\n")
    open(dst, "w").write(render(lines, title))
    print(f"wrote {dst} ({len(lines)} lines)")


if __name__ == "__main__":
    main()
