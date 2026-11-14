import { createApp } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import App from './App.vue'
import './style.css'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: () => import('./views/Dashboard.vue') },
    { path: '/targets', component: () => import('./views/Targets.vue') },
    { path: '/jobs', component: () => import('./views/Jobs.vue') },
    { path: '/settings', component: () => import('./views/Settings.vue') },
    { path: '/logs/:name', component: () => import('./views/Logs.vue') },
    { path: '/login', component: () => import('./views/Login.vue'), meta: { bare: true } },
    { path: '/setup', component: () => import('./views/Setup.vue'), meta: { bare: true } },
    { path: '/:rest(.*)*', redirect: '/' },
  ],
})

createApp(App).use(router).mount('#app')
