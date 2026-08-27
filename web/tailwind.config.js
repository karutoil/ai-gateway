/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{js,ts,jsx,tsx}"],
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        // Aliases to the ORIGINAL tokens kept so pages migrate gradually.
        graphite: "rgb(var(--c-graphite) / <alpha-value>)",
        paper: "rgb(var(--c-paper) / <alpha-value>)",
        stone: "rgb(var(--c-stone) / <alpha-value>)",
        muted: "rgb(var(--c-muted) / <alpha-value>)",
        surface: "rgb(var(--c-surface) / <alpha-value>)",
        app: "rgb(var(--c-graphite) / <alpha-value>)",
        raised: "rgb(var(--c-raised) / <alpha-value>)",
        // Legacy palette (pages not yet migrated still compile).
        amber: "#FFB84D",
        teal: "#2CD9A3",
        cream: "#FDFBF7",
        sand: "#F5F0E6",
        clay: "#E8E0D1",
        ink: "#1E2422",
        danger: "var(--c-danger)",
      },
      fontFamily: {
        sans: ["Inter", "system-ui", "-apple-system", "sans-serif"],
        mono: ["IBM Plex Mono", "ui-monospace", "monospace"],
      },
      boxShadow: {
        card: '0 1px 2px rgb(0 0 0 / 0.18), 0 1px 3px rgb(0 0 0 / 0.10)',
        pop: '0 10px 30px rgb(0 0 0 / 0.35), 0 2px 8px rgb(0 0 0 / 0.25)',
        glow: '0 0 6px rgb(44 217 163 / 0.55)',
      },
      keyframes: {
        pageIn: {
          '0%': { opacity: '0', transform: 'translateY(6px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' },
        },
        fadeIn: { '0%': { opacity: '0' }, '100%': { opacity: '1' } },
        toastIn: {
          '0%': { opacity: '0', transform: 'translateY(8px) scale(0.98)' },
          '100%': { opacity: '1', transform: 'translateY(0) scale(1)' },
        },
        modalIn: {
          '0%': { opacity: '0', transform: 'scale(0.97)' },
          '100%': { opacity: '1', transform: 'scale(1)' },
        },
        shimmer: {
          '0%, 100%': { opacity: '0.45' },
          '50%': { opacity: '0.9' },
        },
        pulseSoft: {
          '0%, 100%': { opacity: '1' },
          '50%': { opacity: '0.4' },
        },
        sidebarIn: {
          '0%': { transform: 'translateX(-16px)', opacity: '0' },
          '100%': { transform: 'translateX(0)', opacity: '1' },
        },
      },
      animation: {
        page: 'pageIn 0.28s cubic-bezier(0.22,1,0.36,1)',
        fade: 'fadeIn 0.15s ease',
        toast: 'toastIn 0.22s cubic-bezier(0.22,1,0.36,1)',
        modal: 'modalIn 0.18s cubic-bezier(0.22,1,0.36,1)',
        shimmer: 'shimmer 1.6s ease-in-out infinite',
        'pulse-soft': 'pulseSoft 2s ease-in-out infinite',
        sidebar: 'sidebarIn 0.25s cubic-bezier(0.22,1,0.36,1)',
      },
    },
  },
  plugins: [],
}
