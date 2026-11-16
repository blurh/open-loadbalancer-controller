package nginx

type HttpConfig struct {
	ListenerPort string
	ServerName   string
	CertFile     string
	CertKeyFile  string
	Backends     []string
}

var templHttpServer = `
server {
    listen      {{ .ListenerPort }} {{ if .CertFile }}ssl{{ end }};
    {{- if .ServerName }}
    server_name {{ .ServerName }};
    {{- end }}

    {{- if .CertFile }}
    ssl_certificate             {{ .CertFile }};
    ssl_certificate_key         {{ .CertKeyFile }};
    ssl_session_timeout         5m;
    ssl_ciphers                 ECDHE-RSA-AES128-GCM-SHA256:ECDHE:ECDH:AES:HIGH:!NULL:!aNULL:!MD5:!ADH:!RC4;
    ssl_protocols               TLSv1 TLSv1.1 TLSv1.2;
    ssl_prefer_server_ciphers   on;
    server_tokens               off;
    {{- end }}

    location / {
        proxy_set_header    Host                $host;
        proxy_set_header    X-Real-IP           $remote_addr;
        proxy_set_header    X-Forwarded-For     $proxy_add_x_forwarded_for;
        proxy_set_header    X-Forwarded-Proto   $scheme;
        proxy_set_header    Upgrade             $http_upgrade;
        proxy_set_header    Connection          $connection_upgrade;
        proxy_http_version  1.1;
        proxy_pass          http://http-backend-{{ .ListenerPort }};
    }
}
`

var templHttpUpstream = `
upstream http-backend-{{ .ListenerPort }} {
    {{- range $i, $backend := .Backends }}
    server {{ $backend }};
    {{- end -}}
}`

type StreamConfig struct {
	Protocol     string
	ListenerPort string
	SoKeepalive  bool
	Backends     []string
}

var templStreamServer = `
server {
    {{- if eq .Protocol "udp" }}
    listen {{ .ListenerPort }} udp reuseport;
    {{- else }}
    listen {{ .ListenerPort }}{{ if .SoKeepalive }} so_keepalive=on{{ end }};
    {{- end }}
    proxy_pass stream-{{ .Protocol }}-backend-{{ .ListenerPort }};
}
`

var templStreamUpstream = `
upstream stream-{{ .Protocol }}-backend-{{ .ListenerPort }} {
    {{- range $i, $backend := .Backends }}
    server {{ $backend }};
    {{- end -}}
}
`
