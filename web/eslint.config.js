import js from '@eslint/js'
import { defineConfig } from 'eslint/config'
import pluginVue from 'eslint-plugin-vue'
import globals from 'globals'
import tseslint from 'typescript-eslint'

export default defineConfig(
  { ignores: ['dist/**', 'node_modules/**'] },
  js.configs.recommended,
  tseslint.configs.recommended,
  pluginVue.configs['flat/recommended'],
  {
    files: ['**/*.vue'],
    languageOptions: { parserOptions: { parser: tseslint.parser } },
  },
  {
    languageOptions: { globals: { ...globals.browser } },
    rules: {
      // Views are named after their route (Dashboard, Jobs, Settings…) and
      // the few shared components are not used as HTML tags: single-word
      // names cannot clash with HTML elements here.
      'vue/multi-word-component-names': 'off',
      // Template layout: the templates use a compact, hand-formatted style
      // (no formatter in this project). These rules only move line breaks
      // and indentation, they catch no bugs.
      'vue/max-attributes-per-line': 'off',
      'vue/singleline-html-element-content-newline': 'off',
      'vue/multiline-html-element-content-newline': 'off',
      'vue/html-indent': 'off',
      'vue/first-attribute-linebreak': 'off',
      'vue/html-closing-bracket-newline': 'off',
      'vue/html-self-closing': 'off',
    },
  },
  {
    // The icon body is one of the component's constant SVG paths, never
    // user input (a comment in the template would add a root node).
    files: ['src/components/Icon.vue'],
    rules: { 'vue/no-v-html': 'off' },
  },
)
