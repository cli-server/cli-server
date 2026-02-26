FROM node:20-slim

WORKDIR /app

COPY package.json package-lock.json* ./
RUN npm install --production

COPY dist/ dist/

ENV NODE_ENV=production
EXPOSE 8081

CMD ["node", "dist/index.js"]
