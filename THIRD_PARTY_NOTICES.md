# Third-party notices

## Lucide icons

The icons in the web UI are from [Lucide](https://lucide.dev), which is available under the ISC License. Parts of Lucide derive from [Feather](https://github.com/feathericons/feather) and are under the MIT License. Both notices are in [third_party/lucide/LICENSE](third_party/lucide/LICENSE), unchanged.

What is vendored: the SVG path data of 42 icons, copied unchanged from the upstream `icons/*.svg` files into one inline sprite, `internal/web/templates/icons.html`. Stroke width, caps and joins are set in CSS (`.ic` in `internal/web/static/app.css`). No Lucide code or package is included.
