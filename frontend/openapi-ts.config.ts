import { defineConfig } from '@hey-api/openapi-ts';

export default defineConfig({
  input: '../protocol/openapi.yaml',
  output: 'src/generated/protocol',
  plugins: ['@hey-api/client-fetch'],
});
