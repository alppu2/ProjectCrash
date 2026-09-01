import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    open: true,
  },
  // jsdom, because the hook under test renders and useChatStream's cleanup
  // runs on unmount. Test globals stay off: every helper is imported, so a
  // reader can see where `describe` and `expect` come from.
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
  },
})
