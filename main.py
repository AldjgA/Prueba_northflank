import os
from contextlib import asynccontextmanager

from aiogram import Bot, Dispatcher, types
from fastapi import FastAPI, Request

# Leemos las credenciales desde el entorno seguro (Northflank las inyectará)
TOKEN = os.getenv("TELEGRAM_BOT_TOKEN")
WEBHOOK_URL = os.getenv("WEBHOOK_URL")

# Inicializamos el bot y el despachador de aiogram
bot = Bot(token=TOKEN)
dp = Dispatcher()


# Manejador básico: responde con el mismo texto que recibe (Eco)
@dp.message()
async def echo_handler(message: types.Message):
    await message.answer(f"Bot en Northflank dice: {message.text}")


# Ciclo de vida de FastAPI: Al arrancar, le decimos a Telegram dónde enviarnos los mensajes
@asynccontextmanager
async def lifespan(app: FastAPI):
    # Startup: Configuramos el Webhook
    if TOKEN and WEBHOOK_URL:
        await bot.set_webhook(url=f"{WEBHOOK_URL}/webhook")
    yield
    # Shutdown: Limpiamos el Webhook al apagar
    if TOKEN:
        await bot.delete_webhook()


app = FastAPI(lifespan=lifespan)


# Este es el puerto de entrada que Telegram golpeará con cada mensaje nuevo
@app.post("/webhook")
async def telegram_webhook(request: Request):
    data = await request.json()
    update = types.Update(**data)
    await dp.feed_update(bot=bot, update=update)
    return {"status": "ok"}


@app.get("/health")
async def health_check():
    return {"status": "bot_alive"}
