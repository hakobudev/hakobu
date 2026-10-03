// Tailwind 3 config for the panel; `go generate ./cmd` rebuilds static/app.css.
// Colors are the design tokens from styles.css and nothing else, so a class
// like bg-zinc-50 doesn't exist: pages use the components defined there.
module.exports = {
  content: ['./*.templ', './ui.go', './static/app.js'],
  theme: {
    colors: {
      transparent: 'transparent',
      current: 'currentColor',
      fg: 'var(--fg)',
      muted: 'var(--fg-muted)',
      accent: 'var(--accent)',
      success: 'var(--success)',
      attention: 'var(--attention)',
      danger: 'var(--danger)',
      canvas: 'var(--bg)',
      subtle: 'var(--bg-muted)',
      line: 'var(--border)',
    },
    extend: {
      fontFamily: {sans: ['Inter', 'ui-sans-serif', 'system-ui'], mono: ['"JetBrains Mono"', 'ui-monospace', 'monospace']},
    },
  },
};
