# storecraft console image: builds web/ and serves web/dist with nginx, which
# also forwards the API to the primary (same origin, so no CORS is needed).
# Build context is the project root:  docker build -f web/Dockerfile .

FROM node:24.15.0-alpine AS build
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web/ ./
RUN npm run build

FROM nginx:1.29.8-alpine
COPY web/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /web/dist /usr/share/nginx/html
EXPOSE 80
