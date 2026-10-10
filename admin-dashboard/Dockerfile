FROM nginx:1.27-alpine
ENV NGINX_ENVSUBST_FILTER=API_URL \
    API_URL=http://localhost:8080
COPY nginx-templates/ /etc/nginx/templates/
COPY docker-entrypoint-config.sh /docker-entrypoint.d/40-config.sh
COPY public/ /usr/share/nginx/html/
COPY src/ /usr/share/nginx/html/src/
