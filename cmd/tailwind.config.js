// Tailwind 3 config for the panel; `go generate ./cmd` rebuilds static/app.css.
module.exports = {
  content: ['./web.html', './static/app.js'],
  theme: {extend: {
    fontFamily: {sans: ['Inter', 'ui-sans-serif', 'system-ui'], mono: ['"JetBrains Mono"', 'ui-monospace', 'monospace']},
    colors: {edge: '#e4e4e7', ink: '#18181b', dim: '#71717a', faint: '#a1a1aa', accent: '#6d5ef7', good: '#16a34a', warn: '#d97706', bad: '#dc2626', console: '#0a0a0b'},
  }},
};
