import { createApp } from 'vue'
import ElementPlus from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import 'element-plus/dist/index.css'

import App from './App.vue'
import HeroSmsPurchasePage from './pages/HeroSmsPurchasePage.vue'
import './styles.css'

const normalizedPath = window.location.pathname.replace(/\/+$/, '') || '/'
const rootComponent = normalizedPath === '/hero-sms' ? HeroSmsPurchasePage : App

createApp(rootComponent).use(ElementPlus, { locale: zhCn }).mount('#app')
