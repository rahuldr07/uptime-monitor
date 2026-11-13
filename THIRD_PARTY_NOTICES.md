# Third-party notices

## Lucide icons

The icons in the web UI are from [Lucide](https://lucide.dev), which is available under the ISC License. Parts of Lucide derive from [Feather](https://github.com/feathericons/feather) and are under the MIT License. Both notices are in [third_party/lucide/LICENSE](third_party/lucide/LICENSE), unchanged.

What is vendored: the SVG path data of 41 icons, copied unchanged from the upstream `icons/*.svg` files into one inline sprite, `internal/web/templates/icons.html`. Stroke width, caps and joins are set in CSS (`.ic` in `internal/web/static/app.css`). The checkbox tick mask in `app.css` also uses Lucide's `check` path. No Lucide code or package is included.

The notice travels with every build: the binary serves it at `/static/licenses.txt` (`internal/web/static/licenses.txt`, the project licence followed by the Lucide notice, kept in sync with `third_party/lucide/LICENSE` by a test), `app.css` starts with a banner pointing to it, and the Docker image copies this file and the licences to `/usr/share/doc/uptime-monitor/`.

## Google and GitHub logos

The Google "G" and GitHub Invertocat logos on the sign-in and sign-up buttons (`internal/web/templates/partials.html`) are trademarks of their owners. They are used only to identify the sign-in providers, and are not covered by the project's MIT licence.
