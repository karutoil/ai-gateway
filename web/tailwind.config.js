/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{js,ts,jsx,tsx}"],
  darkMode: 'class',
  theme: {
    // Ultrawide: the playground can use a wider reading column on 1440p+.
    screens: { sm:'640px', md:'768px', lg:'1024px', xl:'1280px', '2xl':'1536px', '3xl':'1800px' },
    extend: {
      colors: {
        graphite: "rgb(var(--c-graphite) / <alpha-value>)",
        paper: "rgb(var(--c-paper) / <alpha-value>)",
        stone: "rgb(var(--c-stone) / <alpha-value>)",
        muted: "rgb(var(--c-muted) / <alpha-value>)",
        surface: "rgb(var(--c-surface) / <alpha-value>)",
        app: "rgb(var(--c-graphite) / <alpha-value>)",
        raised: "rgb(var(--c-raised) / <alpha-value>)",
        amber: "rgb(var(--c-amber) / <alpha-value>)",
        good: "rgb(var(--c-good) / <alpha-value>)",
        accent: "rgb(var(--c-accent) / <alpha-value>)",
        onaccent: "rgb(var(--c-onaccent) / <alpha-value>)",
        ink: "rgb(var(--c-ink) / <alpha-value>)",
        cream: "rgb(var(--c-cream) / <alpha-value>)",
        danger: "rgb(var(--c-danger) / <alpha-value>)",
      },
      fontFamily: {
        sans: ["Inter", "system-ui", "-apple-system", "sans-serif"],
        display: ["Inter", "system-ui", "sans-serif"],
        mono: ["JetBrains Mono", "IBM Plex Mono", "ui-monospace", "monospace"],
      },
      boxShadow: {
        card: 'var(--shadow-card, 0 1px 2px rgb(0 0 0 / 0.08))',
        pop: 'var(--shadow-pop, 0 16px 40px -12px rgb(0 0 0 / 0.25))',
      },
      keyframes: {
        pageIn: {
          '0%': { opacity: '0', transform: 'translateY(4px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' },
        },
        fadeIn: { '0%': { opacity: '0' }, '100%': { opacity: '1' } },
        toastIn: {
          '0%': { opacity: '0', transform: 'translateY(8px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' },
        },
        modalIn: {
          '0%': { opacity: '0', transform: 'translateY(8px) scale(0.985)' },
          '100%': { opacity: '1', transform: 'translateY(0) scale(1)' },
        },
        shimmer: {
          '0%, 100%': { opacity: '0.45' },
          '50%': { opacity: '0.9' },
        },
        pulseSoft: {
          '0%, 100%': { opacity: '1' },
          '50%': { opacity: '0.35' },
        },
        sidebarIn: {
          '0%': { transform: 'translateX(-16px)', opacity: '0' },
          '100%': { transform: 'translateX(0)', opacity: '1' },
        },
      },
      animation: {
        page: 'pageIn 0.2s ease-out',
        fade: 'fadeIn 0.15s ease',
        toast: 'toastIn 0.2s ease-out',
        modal: 'modalIn 0.18s ease-out',
        shimmer: 'shimmer 1.6s ease-in-out infinite',
        'pulse-soft': 'pulseSoft 2s ease-in-out infinite',
        sidebar: 'sidebarIn 0.2s ease-out',
      },
    },
  },
  plugins: [],
}
