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

En el panel entra a **Clientes**, registra una instancia (por ejemplo `gpu_1`) y **copia el token que aparece una sola vez**. No existe clave global compartida: cada instancia tiene una credencial única, almacenada como hash en SQLite.

```bash
export BASE="http://localhost:8080"
export TOKEN="<token-individual-generado-en-Clientes>"
curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/api/v1/reference/meta"
curl -fsS -H "Authorization: Bearer $TOKEN" -o reference.wav "$BASE/api/v1/reference"

curl -i -X POST "$BASE/api/v1/jobs/claim" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{}'
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
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"lease_token":"TOKEN_ALEATORIO"}'
```

Al terminar, sube el audio resultante como multipart:

```bash
curl -X POST "$BASE/api/v1/jobs/1/complete" \
  -H "Authorization: Bearer $TOKEN" \
  -F "lease_token=TOKEN_ALEATORIO" -F "audio=@output.wav;type=audio/wav"
```

Ante error reportado por cliente:

```bash
curl -X POST "$BASE/api/v1/jobs/1/fail" \
 -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
 -d '{"lease_token":"TOKEN_ALEATORIO","error":"Error de síntesis"}'
```

Si la reserva ya terminó, expiró o pertenece a otro cliente, los endpoints de heartbeat/completado/fallo devuelven **409**. No deben reintentar esa subida: deben pedir otra tarea.

También están disponibles `GET /api/v1/client/me` y `GET /api/v1/stats`. La interfaz web requiere login y los formularios incluyen protección CSRF. Los workers usan únicamente la clave Bearer.

## Panel de administración

- **Resumen**: totales, pendientes, en proceso, terminados, errores y barra de progreso.
- **Trabajos**: lista paginada con filtros y reintento manual de errores.
- **Clientes**: dar de alta una GPU/cliente y recibir su token individual, renovar o revocar credenciales. Historial de actividad y trabajo.
- **Documentación API**: guía interactiva en el panel con endpoints, curl, estados y respuestas.
- **Configuración**: subir CSV y WAV de referencia (con reproductor).
- **Descargar ZIP**: genera un ZIP al momento con WAVs completados y CSV.

Los nombres WAV deben ser sencillos, por ejemplo `5.wav`. Se impide subir rutas, archivos demasiado grandes o nombres peligrosos. Los nombres duplicados al importar se omiten. Solo un servidor puede escribir esta base de datos; copias simultáneas de la aplicación sobre distintos nodos requerirían coordinación externa o cambio de base de datos.

## Pruebas

```bash
go test ./... -count=1
go vet ./...
go test -race ./...
```

Las pruebas cubren cinco reservas simultáneas, recuperación, registro de clientes, validación de identidad, revocación y renovación de tokens, CSRF, inicio de sesión, importación CSV, subida de referencia, carga del WAV resultante y estructura del ZIP.

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

## Gestión de credenciales de clientes

Accede a `/workers` para crear un cliente por instancia y obtener el token `dts_...` de 256 bits. El token completo **solo se muestra una vez**. Si se pierde, usa «Renovar token». «Revocar» desactiva el acceso. Ambas acciones liberan inmediatamente sus trabajos en proceso para reintento y no afectan a los archivos terminados. Las claves anteriores dejan de tener validez.

Los clientes se identifican exclusivamente por su token, así que `POST /api/v1/jobs/claim` acepta `{}` y no necesita `worker_id`; un ID ajeno produce HTTP 403. El cliente puede consultar su identidad con `GET /api/v1/client/me`. Las rutas administrativas están protegidas por sesión y CSRF; una clave de cliente no las habilita.

**Migración:** la antigua variable `WORKER_API_KEY` fue eliminada porque una sola clave compartida impediría revocación selectiva. Las instalaciones existentes deben registrar sus instancias en el panel y actualizar la variable `TOKEN` de cada cliente. La base SQLite previa se conserva y la tabla `clients` se crea automáticamente al iniciar. Consulta `/api-docs` para los ejemplos completos.

### Importación CSV con revisión previa

En **Configuración → Importar dataset CSV**, el panel analiza el archivo antes de modificar SQLite. El lector admite el separador `|` o coma, UTF-8 (con o sin BOM), comillas correctamente escapadas y comillas literales dentro del texto. Si el CSV necesita recuperación, se muestra una advertencia con las líneas detectadas; un registro imposible de corregir (por ejemplo, sin nombre WAV o sin texto) se rechaza con su línea.

En la vista previa también se notifican nombres de WAV repetidos dentro del CSV, nombres que ya existen en la base de datos y textos repetidos aunque tengan otro nombre de WAV. Los nombres duplicados solo se omiten tras confirmar expresamente; **los registros ya existentes nunca se eliminan ni sobrescriben**. Para frases duplicadas, el administrador decide si omitirlas o conservarlas. La vista previa caduca a los 30 minutos y está vinculada a la sesión que la creó.

La importación conserva los **bytes originales** de cada CSV confirmado en `DATA_DIR/sources/` y registra su SHA-256, nombre, fecha, cantidad importada y omitida en SQLite. Al descargar `/export`, el ZIP incluye `metadata.csv`, los WAV completados y los originales importados bajo `sources/<id>-<nombre>.csv`. Los CSV importados antes de esta versión no pueden recuperarse del historial de SQLite, por lo que no aparecerán como originales en la descarga retroactiva.
