import { mount } from 'svelte'
import './app.css'
import App from './App.svelte'
import { initLocale } from './lib/i18n'

// Resolve the locale and set <html lang> before the first render so messages
// and the font stack are correct on first paint (design A34 §2).
initLocale()

const app = mount(App, {
  target: document.getElementById('app')!,
})

export default app
