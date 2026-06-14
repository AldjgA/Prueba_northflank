# Usamos una imagen oficial y ligera de Python
FROM python:3.11-slim

# Establecemos el directorio de trabajo dentro del contenedor
WORKDIR /app

# Copiamos solo el archivo de requerimientos primero (optimiza la caché de Docker)
COPY requirements.txt .

# Instalamos las dependencias
RUN pip install --no-cache-dir -r requirements.txt

# Copiamos el resto del código
COPY main.py .

# Exponemos el puerto 8080 (Estándar para microservicios web)
EXPOSE 8080

# Comando para iniciar el servidor.
# IMPORTANTE: host 0.0.0.0 es obligatorio para que funcione fuera de localhost
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8080"]
