import os

from aiogram import Bot, Dispatcher, types
from fastapi import BackgroundTasks, FastAPI, Request
from google import genai

# Credenciales inyectadas por el entorno
TOKEN = os.getenv("clave1")
GEMINI_API_KEY = os.getenv("clave2")

# Inicialización de clientes
bot = Bot(token=TOKEN)
dp = Dispatcher()
app = FastAPI()

# Inicializamos el cliente de Gemini
gemini_client = genai.Client(api_key=GEMINI_API_KEY)

# Aquí debes colocar exactamente el nombre del modelo que te muestra AI Studio
MODEL_ID = "gemma-4-31b-it"


@dp.message()
async def gemini_handler(message: types.Message):
    # UX: Le mostramos al usuario que el bot está "escribiendo..."
    await bot.send_chat_action(chat_id=message.chat.id, action="typing")

    try:
        # Llamada asíncrona a la API de Gemini
        response = await gemini_client.aio.models.generate_content(
            model=MODEL_ID, contents=message.text
        )

        # Enviamos la respuesta generada a Telegram
        await message.answer(response.text)

    except Exception as e:
        # Manejo de errores básico para no dejar al usuario en visto
        await message.answer(f"Ocurrió un error al procesar tu consulta: {str(e)}")


@app.post("/webhook")
async def telegram_webhook(request: Request, background_tasks: BackgroundTasks):
    data = await request.json()
    update = types.Update(**data)
    background_tasks.add_task(dp.feed_update, bot=bot, update=update)
    return {"status": "ok"}


@app.get("/health")
async def health_check():
    return {"status": "bot_alive"}
