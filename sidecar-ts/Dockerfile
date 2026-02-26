FROM node:20-slim

RUN corepack enable && corepack prepare pnpm@9.15.0 --activate

WORKDIR /app

COPY package.json pnpm-lock.yaml* ./
RUN pnpm install --prod --frozen-lockfile || pnpm install --prod

COPY dist/ dist/

ENV NODE_ENV=production
EXPOSE 8081

CMD ["node", "dist/index.js"]
