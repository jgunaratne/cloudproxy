import { defineConfig, Plugin } from 'vite';
import { execSync } from 'child_process';

const CLOUD_RUN_URL = process.env.CLOUD_RUN_URL;
if (!CLOUD_RUN_URL) {
  throw new Error(
    'CLOUD_RUN_URL is not set. Copy viewer/.env.example to viewer/.env and fill in your values.',
  );
}

function getIdentityToken(): string {
  return execSync('gcloud auth print-identity-token 2>/dev/null', {
    encoding: 'utf-8',
    timeout: 10000,
  }).trim();
}

/**
 * Vite plugin that proxies WebSocket connections to Cloud Run and
 * injects a Google identity token for IAM authentication.
 *
 * The browser WebSocket API cannot set custom headers, so the Vite
 * dev server acts as a local proxy:
 *   Browser → ws://localhost:3000/ws → (adds Authorization header) → wss://Cloud-Run/ws
 *
 * Also exposes GET /api/token for debugging.
 */
function gcloudAuthPlugin(): Plugin {
  return {
    name: 'gcloud-auth',
    configureServer(server) {
      // Debug endpoint to check the token
      server.middlewares.use('/api/token', (_req, res) => {
        try {
          const token = getIdentityToken();
          res.setHeader('Content-Type', 'application/json');
          res.setHeader('Cache-Control', 'no-store');
          res.end(JSON.stringify({ token, audience: CLOUD_RUN_URL }));
        } catch (err: any) {
          console.error('[gcloud-auth] failed:', err.message);
          res.statusCode = 500;
          res.setHeader('Content-Type', 'application/json');
          res.end(JSON.stringify({ error: 'Failed to get identity token. Run: gcloud auth login' }));
        }
      });
    },
  };
}

export default defineConfig({
  plugins: [gcloudAuthPlugin()],
  server: {
    port: 3000,
    open: true,
    proxy: {
      // Use /cloudproxy-ws to avoid conflicts with Vite's own HMR WebSocket.
      // The viewer should connect to ws://localhost:3000/cloudproxy-ws
      '/cloudproxy-ws': {
        target: CLOUD_RUN_URL,
        changeOrigin: true,
        secure: true,
        ws: true,
        // Rewrite /cloudproxy-ws to /ws on the target server.
        rewrite: (path) => path.replace(/^\/cloudproxy-ws/, '/ws'),
        configure: (proxy) => {
          proxy.on('proxyReqWs', (proxyReq, req) => {
            try {
              const token = getIdentityToken();
              proxyReq.setHeader('Authorization', `Bearer ${token}`);
              console.log(`[ws-proxy] injected identity token, proxying: ${req.url}`);
            } catch (err: any) {
              console.error('[ws-proxy] failed to get identity token:', err.message);
            }
          });
          proxy.on('error', (err, _req, _res) => {
            console.error('[ws-proxy] error:', err.message);
          });
          proxy.on('open', () => {
            console.log('[ws-proxy] WebSocket connection opened to Cloud Run');
          });
          proxy.on('close', () => {
            console.log('[ws-proxy] WebSocket connection closed');
          });
        },
      },
    },
  },
  build: {
    target: 'es2020',
  },
});
