import os

from aiogram import Bot, Dispatcher, types
from fastapi import FastAPI, Request

# es para el sugar bot
TOKEN = os.getenv("clave1")

bot = Bot(token=TOKEN)
dp = Dispatcher()

app = FastAPI()


@dp.message()
async def echo_handler(message: types.Message):
    await message.answer(f"Bot dice: {message.text}")


@app.post("/webhook")
async def telegram_webhook(request: Request):
    data = await request.json()
    update = types.Update(**data)
    await dp.feed_update(bot=bot, update=update)
    return {"status": "ok"}


@app.get("/health")
async def health_check():
    return {"status": "bot_alive"}
