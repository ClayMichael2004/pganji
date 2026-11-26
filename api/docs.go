package api

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.yaml
var openAPISpec []byte

const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>Pganji Enterprise Ledger — API Reference</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css" />
  <style>
    body {
      margin: 0;
      background-color: #09090b;
      color: #e4e4e7;
    }
    .swagger-ui .topbar {
      background-color: #111114;
      border-bottom: 1px solid #27272a;
    }
    .swagger-ui .info .title,
    .swagger-ui .info h1, .swagger-ui .info h2, .swagger-ui .info h3,
    .swagger-ui .opblock-tag,
    .swagger-ui .opblock .opblock-summary-operation-id,
    .swagger-ui .opblock .opblock-summary-path,
    .swagger-ui .opblock .opblock-summary-description,
    .swagger-ui table thead tr td, .swagger-ui table thead tr th,
    .swagger-ui .parameter__name, .swagger-ui .parameter__type,
    .swagger-ui .response-col_status, .swagger-ui .response-col_description {
      color: #f4f4f5;
    }
    .swagger-ui .scheme-container {
      background-color: #111114;
      box-shadow: none;
      border-bottom: 1px solid #27272a;
    }
    .swagger-ui .info p, .swagger-ui .info li, .swagger-ui .renderedMarkdown p {
      color: #a1a1aa;
    }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
  <script>
    window.onload = () => {
      window.ui = SwaggerUIBundle({
        url: '/api/v1/openapi.yaml',
        dom_id: '#swagger-ui',
        deepLinking: true,
        persistAuthorization: true,
        presets: [
          SwaggerUIBundle.presets.apis,
          SwaggerUIBundle.SwaggerUIStandalonePreset
        ],
        layout: 'BaseLayout'
      });
    };
  </script>
</body>
</html>`

// HandleOpenAPISpec serves the embedded OpenAPI 3.1 YAML specification.
func (s *Server) HandleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPISpec)
}

// HandleSwaggerUI serves an interactive Swagger UI documentation portal backed by /api/v1/openapi.yaml.
func (s *Server) HandleSwaggerUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(swaggerUIHTML))
}
