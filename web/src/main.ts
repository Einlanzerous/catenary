import { createApp } from 'vue'
import App from './App.vue'
import { select, setTheme, state } from './store'
import './styles/base.css'

// Dark is primary; light is derived. Honour an explicit OS preference, but
// default to dark rather than to whatever the machine happens to say.
if (window.matchMedia('(prefers-color-scheme: light)').matches) {
  setTheme('light')
} else {
  setTheme(state.theme)
}

// The conversation you open with is, by definition, one you are reading — so
// it carries no badge. Its "N NEW" rule stays put for the visit.
select(state.activeId)

createApp(App).mount('#app')
