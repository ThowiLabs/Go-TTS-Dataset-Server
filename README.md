# Dataset TTS Server

Servidor **Go** para coordinar la generación distribuida de audios de datasets TTS. Incluye API REST, SQLite en modo WAL, reservas exclusivas, recuperación por vencimiento, voz de referencia compartida y panel de administración basado en los **archivos oficiales de AdminLTE 4.10.1**.

## Características

- Importa un archivo CSV con formato `1.wav|Texto correspondiente`. También admite CSV tradicional separado por coma; respeta comillas de CSV.
- Administra el estado de cada tarea: pendiente, en proceso, terminado o error, con historial de intentos y mensaje de error.
- Distribuye trabajo en orden mediante `POST /api/v1/jobs/claim` con operación SQLite atómica, sin asignar dos veces la misma tarea a diferentes workers.
- Permite que los workers renueven una reserva mediante heartbeat; recupera automáticamente reservas vencidas cada 15 segundos y al reclamar nuevos trabajos.
- Al agotar `MAX_ATTEMPTS`, conserva el trabajo en error para revisión y reintento manual.
- Almacena WAVs en disco y sus rutas en SQLite. Exporta `dataset.zip` con `metadata.csv` y `wavs/<nombre>.wav`, únicamente con los registros completados.
- Mantiene un WAV de referencia descargable y su hash SHA-256 para caché local de los clientes.
- Panel AdminLTE con vistas dinámicas renderizadas en Go; sin procesos Node/Python en tiempo de ejecución.

## Instalación

Requiere **Go 1.24 o superior**.

```bash
cp .env.example .env
# Editar .env para proporcionar credenciales seguras
go mod download
go build -o dataset-server .
./dataset-server
```

Visita http://localhost:8080 y accede con `ADMIN_USER` y `ADMIN_PASSWORD`.

Variables de entorno del proceso tienen prioridad sobre `.env`. Se puede definir `CONFIG_FILE` para otro archivo. `DATA_DIR` debe estar en un disco persistente. No inicies múltiples servidores independientes sobre el mismo directorio de datos; sí puedes usar múltiples **clientes workers** contra un único servidor.

## Organización

```text
main.go                         Configuración, sesiones y rutas
store.go                        SQLite, reserva atómica, estados, recuperación
api.go                          Importación, referencia, workers y exportación ZIP
ui.go                           Autenticación y controladores del panel
web/templates/layouts/          Estructura HTML común y pantalla de acceso
web/templates/content/          Contenido variable de cada ruta
web/static/vendor/              AdminLTE original + dependencias frontend
contexto/ y tareas/             Estado de proyecto (protegido antes del commit)
data/                           SQLite, WAVs, referencia y staging (ignorado por Git)
```

## Ejemplo de API para cinco workers

Usa una **clave de worker** independiente de las credenciales del administrador:

```bash
export BASE="http://localhost:8080"
export WORKER_API_KEY="<valor-de-WORKER_API_KEY-del-servidor>"
curl -fsS -H "Authorization: Bearer $WORKER_API_KEY" "$BASE/api/v1/reference/meta"
curl -fsS -H "Authorization: Bearer $WORKER_API_KEY" -o reference.wav "$BASE/api/v1/reference"

curl -i -X POST "$BASE/api/v1/jobs/claim" \
  -H "Authorization: Bearer $WORKER_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"worker_id":"gpu_1"}'
```

Una reserva devuelve HTTP 200 con estructura:

```json
{
  "job": {
    "id": 1, "filename": "1.wav", "text": "Hola", "status": "processing",
    "attempts": 1, "worker_id": "gpu_1", "lease_until": 1800000000,
    "lease_token": "TOKEN_ALEATORIO"
  },
  "reference_sha256": "SHA256_DEL_WAV",
  "reference_url": "/api/v1/reference",
  "lease_seconds": 1800
}
```

Cuando no quedan trabajos pendientes, responde HTTP **204**. Si aún no hay referencia de voz, responde HTTP **409**.

Mientras procesa, el worker debe enviar un heartbeat antes de que venza la reserva:

```bash
curl -X POST "$BASE/api/v1/jobs/1/heartbeat" \
  -H "Authorization: Bearer $WORKER_API_KEY" -H "Content-Type: application/json" \
  -d '{"lease_token":"TOKEN_ALEATORIO"}'
```

Al terminar, sube el audio resultante como multipart:

```bash
curl -X POST "$BASE/api/v1/jobs/1/complete" \
  -H "Authorization: Bearer $WORKER_API_KEY" \
  -F "lease_token=TOKEN_ALEATORIO" -F "audio=@output.wav;type=audio/wav"
```

Ante error reportado por cliente:

```bash
curl -X POST "$BASE/api/v1/jobs/1/fail" \
 -H "Authorization: Bearer $WORKER_API_KEY" -H "Content-Type: application/json" \
 -d '{"lease_token":"TOKEN_ALEATORIO","error":"Error de síntesis"}'
```

Si la reserva ya terminó, expiró o pertenece a otro cliente, los endpoints de heartbeat/completado/fallo devuelven **409**. No deben reintentar esa subida: deben pedir otra tarea.

También está disponible `GET /api/v1/stats`. La interfaz web requiere login y los formularios incluyen protección CSRF. Los workers usan únicamente la clave Bearer.

## Panel de administración

- **Resumen**: totales, pendientes, en proceso, terminados, errores y barra de progreso.
- **Trabajos**: lista paginada con filtros y reintento manual de errores.
- **Instancias**: última actividad registrada y trabajos asignados/terminados por cliente.
- **Configuración**: subir CSV y WAV de referencia (con reproductor).
- **Descargar ZIP**: genera un ZIP al momento con WAVs completados y CSV.

Los nombres WAV deben ser sencillos, por ejemplo `5.wav`. Se impide subir rutas, archivos demasiado grandes o nombres peligrosos. Los nombres duplicados al importar se omiten. Solo un servidor puede escribir esta base de datos; copias simultáneas de la aplicación sobre distintos nodos requerirían coordinación externa o cambio de base de datos.

## Pruebas

```bash
go test ./... -count=1
go vet ./...
go test -race ./...
```

Las pruebas cubren cinco reservas simultáneas, recuperación, rechazo de tokens incorrectos, inicio de sesión, importación CSV, subida de referencia, carga del WAV resultante y estructura del ZIP.

## Avisos

- El servidor **no ejecuta TTS**. Los cinco clientes ejecutan la inferencia y suben archivos generados.
- Las sesiones web y el token de worker deben protegerse con HTTPS al usar un túnel público o proxy inverso.
- La voz de referencia puede reemplazarse. Los clientes pueden comparar su SHA-256 para saber si deben descargarla de nuevo.
- Usa backups del directorio de datos, incluyendo SQLite y WAVs. El ZIP solo contiene audios completados.
- Cambiar el texto de un archivo ya importado requiere gestionar el registro existente; por seguridad, la importación no sobrescribe tareas anteriores.

## Licencias y atribuciones

AdminLTE 4.10.1 se descargó del [release oficial](https://github.com/ColorlibHQ/AdminLTE/releases/tag/v4.10.1) y se usan su CSS, JavaScript y logotipo. Se conservan su identidad y estructura de sidebar, navbar, cards, login y footer. AdminLTE (MIT), Bootstrap (MIT) y Bootstrap Icons (MIT) pertenecen a sus autores respectivos.

## Contexto de desarrollo

La documentación de continuidad se almacena cifrada en `agents.ctx`. Se abre con la herramienta de contexto de Cl antes de continuar con el proyecto; `contexto/` y `tareas/` están excluidas del control de versiones en texto plano.
