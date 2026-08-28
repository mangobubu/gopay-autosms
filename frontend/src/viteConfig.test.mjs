import assert from 'node:assert/strict'
import test from 'node:test'

import config from '../vite.config.ts'

test('Vite API proxy rewrites the development Origin for backend CSRF validation', () => {
  const apiProxy = config.server?.proxy?.['/api']
  assert.equal(apiProxy?.target, 'http://127.0.0.1:8080')
  assert.equal(typeof apiProxy?.configure, 'function')

  let proxyRequestHandler
  apiProxy.configure({
    on(event, handler) {
      if (event === 'proxyReq') proxyRequestHandler = handler
    },
  })
  assert.equal(typeof proxyRequestHandler, 'function')

  const headers = new Map()
  proxyRequestHandler(
    { setHeader(name, value) { headers.set(name, value) } },
    { headers: { origin: 'http://localhost:5173' } },
  )
  assert.equal(headers.get('origin'), 'http://localhost:8080')
})

test('Vite API proxy does not invent an Origin for originless requests', () => {
  const apiProxy = config.server.proxy['/api']
  let proxyRequestHandler
  apiProxy.configure({ on(_event, handler) { proxyRequestHandler = handler } })

  let writes = 0
  proxyRequestHandler(
    { setHeader() { writes += 1 } },
    { headers: {} },
  )
  assert.equal(writes, 0)
})
