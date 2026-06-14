from fastapi import FastAPI

app = FastAPI()

@app.get("/")
async def root():
    return {"mensaje": "¡Hola Northflank! El servidor está vivo y listo para recibir Webhooks en el futuro."}

@app.get("/health")
async def health_check():
    return {"status": "ok"}
