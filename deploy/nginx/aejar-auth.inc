# Shared by both servers: oauth2-proxy endpoints.
location /oauth2/ {
    proxy_pass http://oauth2-proxy:4180;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto https;
    proxy_set_header X-Forwarded-Host $host;
    proxy_set_header X-Auth-Request-Redirect $request_uri;
}
location = /oauth2/auth {
    internal;
    proxy_pass http://oauth2-proxy:4180;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto https;
    proxy_set_header X-Forwarded-Host $host;
    proxy_pass_request_body off;
    proxy_set_header Content-Length "";
}
