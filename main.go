// main.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"io/ioutil"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// --- Структуры для парсинга JSON ответа от WB API ---
// Мы определяем только те поля, которые нам нужны.

type PriceInfo struct {
	Product   int `json:"product"`
	Logistics int `json:"logistics"`
}

type Size struct {
	Name  string    `json:"name"`
	Price PriceInfo `json:"price"`
}

type Product struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Sizes []Size `json:"sizes"`
}

type ProductData struct {
	Products []Product `json:"products"`
}

// type ApiResponse struct {
// 	Data ProductData `json:"data"`
// }

// --- Хранилище отслеживаемых товаров ---
// Для простоты будем хранить все в памяти.
// Структура: map[chatId]map[article]map[size]lastPrice
var trackingData = make(map[int64]map[string]map[string]float64)
var mu sync.RWMutex // Mutex для безопасного доступа к trackingData из разных горутин


const dataFileName = "tracking.json" // Имя файла для хранения данных

// saveDataToFile сохраняет текущее состояние trackingData в JSON-файл.
func saveDataToFile() error {
	mu.RLock() // Блокируем на чтение, чтобы данные не изменились во время сохранения
	defer mu.RUnlock()

	// Преобразуем наши данные (map) в JSON формат (срез байт)
	dataBytes, err := json.MarshalIndent(trackingData, "", "  ") // MarshalIndent для красивого вывода в файле
	if err != nil {
		return fmt.Errorf("ошибка при маршалинге данных в JSON: %w", err)
	}

	// Записываем срез байт в файл, перезаписывая его, если он существует
	err = ioutil.WriteFile(dataFileName, dataBytes, 0644) // 0644 - стандартные права доступа
	if err != nil {
		return fmt.Errorf("ошибка при записи данных в файл: %w", err)
	}

	log.Println("Данные успешно сохранены в", dataFileName)
	return nil
}

// loadDataFromFile загружает данные из JSON-файла в trackingData.
func loadDataFromFile() error {
	mu.Lock() // Блокируем на запись, так как будем полностью перезаписывать map
	defer mu.Unlock()

	// Читаем данные из файла
	dataBytes, err := ioutil.ReadFile(dataFileName)
	if err != nil {
		// Если файл просто не существует, это не ошибка. Просто начинаем с пустыми данными.
		if os.IsNotExist(err) {
			log.Println("Файл данных не найден, начинаем с чистого листа.")
			trackingData = make(map[int64]map[string]map[string]float64)
			return nil
		}
		return fmt.Errorf("ошибка при чтении файла данных: %w", err)
	}

	// Если файл пустой, тоже не делаем ничего
	if len(dataBytes) == 0 {
		log.Println("Файл данных пуст, начинаем с чистого листа.")
		trackingData = make(map[int64]map[string]map[string]float64)
		return nil
	}

	// Преобразуем JSON из файла обратно в нашу map
	err = json.Unmarshal(dataBytes, &trackingData)
	if err != nil {
		return fmt.Errorf("ошибка при анмаршалинге JSON в данные: %w", err)
	}
	
	log.Println("Данные успешно загружены из", dataFileName)
	return nil
}

// --- Клиент для API Wildberries ---

func getWBProductInfo(article string) (*Product, error) {
	// Формируем URL.
	url := fmt.Sprintf("https://card.wb.ru/cards/v4/detail?appType=1&curr=byn&dest=-8144334&spp=30&nm=%s", article)

	// 1. Создаем новый HTTP клиент.
	// Это не обязательно, но хорошая практика, если нужно будет задать таймауты.
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	// 2. Создаем заготовку запроса (request).
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка при создании запроса: %w", err)
	}

	// 3. САМОЕ ВАЖНОЕ: Добавляем заголовки, чтобы выглядеть как браузер.
	req.Header.Add("Accept", "application/json, text/plain, */*")
	req.Header.Add("Accept-Language", "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7")
	req.Header.Add("Connection", "keep-alive")
	// Этот заголовок часто является ключевым
	req.Header.Add("Referer", fmt.Sprintf("https://www.wildberries.by/catalog/%s/detail.aspx", article))
	req.Header.Add("Sec-Fetch-Dest", "empty")
	req.Header.Add("Sec-Fetch-Mode", "cors")
	req.Header.Add("Sec-Fetch-Site", "cross-site")
	// Один из самых важных заголовков. Представляемся браузером.
	req.Header.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36")

	// 4. Выполняем наш настроенный запрос.
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка при выполнении запроса к WB API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Для отладки посмотрим тело ответа даже при ошибке
		bodyBytes, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("WB API вернул статус: %s, тело ответа: %s", resp.Status, string(bodyBytes))
	}

	// --- Начало блока для отладки ---
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ошибка при чтении тела ответа: %w", err)
	}
	bodyString := string(bodyBytes)
	fmt.Println("Ответ от WB API (с заголовками):")
	fmt.Println(bodyString)
	// --- Конец блока для отладки ---

	// Создаем новый ридер для декодера, так как мы прочитали тело ответа
	bodyReader := bytes.NewReader(bodyBytes)

	var apiResponse ProductData
	if err := json.NewDecoder(bodyReader).Decode(&apiResponse); err != nil {
		return nil, fmt.Errorf("ошибка при декодировании JSON: %w", err)
	}

	if len(apiResponse.Products) == 0 {
		return nil, fmt.Errorf("товар с артикулом %s не найден (ответ от сервера был пустым)", article)
	}

	return &apiResponse.Products[0], nil
}

// Вспомогательная функция для расчета и форматирования цены
func calculatePrice(price PriceInfo) float64 {
	return float64(price.Product+price.Logistics) / 100.0
}

func main() {
	// ЗАГРУЖАЕМ ДАННЫЕ ПРИ СТАРТЕ
    if err := loadDataFromFile(); err != nil {
        log.Panicf("Критическая ошибка: не удалось загрузить данные: %v", err)
    }

	// ЗАПУСКАЕМ ВЕБ-СЕРВЕР ДЛЯ RENDER 
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // Порт по умолчанию для локального запуска
	}
	go func() {
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "Wildberries Price Bot is running!")
		})
		log.Printf("Starting health check web server on port %s", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Fatalf("failed to start web server: %v", err)
		}
	}()

	// Получаем токен из переменной окружения
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Panic("TELEGRAM_BOT_TOKEN не установлен!")
	}

	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = true
	log.Printf("Авторизован как %s", bot.Self.UserName)

	// Запускаем фоновую проверку цен в отдельной горутине
	go startPriceChecker(bot)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	// Основной цикл для обработки сообщений от пользователя
	for update := range updates {
		if update.Message == nil {
			continue
		}

		chatID := update.Message.Chat.ID
		msgText := update.Message.Text

		if update.Message.IsCommand() {
			switch update.Message.Command() {
			case "start":
				reply := "Привет! Отправь мне артикул товара с Wildberries, и я начну отслеживать его цену. Например, `310166233`.\n\nИспользуй команду /track [артикул] или просто отправь артикул."
				msg := tgbotapi.NewMessage(chatID, reply)
				msg.ParseMode = "Markdown"
				bot.Send(msg)
			case "track":
				article := update.Message.CommandArguments()
				handleTrackingRequest(bot, chatID, article)
			case "list":
				handleListRequest(bot, chatID)
			case "untrack":
				article := update.Message.CommandArguments()
				handleUntrackRequest(bot, chatID, article)
			default:
				msg := tgbotapi.NewMessage(chatID, "Неизвестная команда.")
				bot.Send(msg)
			}
		} else {
			// Если сообщение - не команда, считаем, что это артикул
			handleTrackingRequest(bot, chatID, msgText)
		}
	}
}

// Обработка запроса на отслеживание
// ПРАВИЛЬНАЯ ВЕРСИЯ handleTrackingRequest
func handleTrackingRequest(bot *tgbotapi.BotAPI, chatID int64, article string) {
	article = strings.TrimSpace(article)
	if _, err := strconv.Atoi(article); err != nil {
		bot.Send(tgbotapi.NewMessage(chatID, "Пожалуйста, отправьте корректный числовой артикул."))
		return
	}

	bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("Проверяю информацию по артикулу %s...", article)))

	product, err := getWBProductInfo(article)
	if err != nil {
		bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("Не удалось получить информацию о товаре: %s", err.Error())))
		return
	}

	// --- Начало критической секции ---
	mu.Lock()
	if _, ok := trackingData[chatID]; !ok {
		trackingData[chatID] = make(map[string]map[string]float64)
	}
	if _, ok := trackingData[chatID][article]; !ok {
		trackingData[chatID][article] = make(map[string]float64)
	}

	var responseText strings.Builder
	responseText.WriteString(fmt.Sprintf("Начинаю отслеживать товар: *%s*\n\nТекущие цены:\n", product.Name))

	for _, size := range product.Sizes {
		price := calculatePrice(size.Price)
		trackingData[chatID][article][size.Name] = price
		responseText.WriteString(fmt.Sprintf("Размер *%s*: `%.2f BYN`\n", size.Name, price))
	}
	mu.Unlock()
	// --- Конец критической секции. Мьютекс свободен! ---

	responseText.WriteString("\nЯ сообщу, если цена на какой-либо из размеров снизится.")
	
	msg := tgbotapi.NewMessage(chatID, responseText.String())
	msg.ParseMode = "Markdown"
	bot.Send(msg)

	// Теперь мы можем безопасно вызывать saveDataToFile, т.к. мьютекс уже разблокирован
	if err := saveDataToFile(); err != nil {
		log.Printf("ОШИБКА: не удалось сохранить данные после добавления товара: %v", err)
		bot.Send(tgbotapi.NewMessage(chatID, "Внимание: произошла ошибка при сохранении вашего товара, он может не отслеживаться после перезапуска."))
	}
}

// Фоновый процесс для периодической проверки цен
func startPriceChecker(bot *tgbotapi.BotAPI) {
	// Проверяем цены каждые 30 минут. Не стоит делать это слишком часто.
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	for {
		<-ticker.C
		log.Println("Запущена периодическая проверка цен...")

		mu.RLock()
		// Копируем данные, чтобы не держать лок на время сетевых запросов
		currentTracking := make(map[int64]map[string]map[string]float64)
		for chatID, articles := range trackingData {
			currentTracking[chatID] = make(map[string]map[string]float64)
			for article, sizes := range articles {
				currentTracking[chatID][article] = make(map[string]float64)
				for size, price := range sizes {
					currentTracking[chatID][article][size] = price
				}
			}
		}
		mu.RUnlock()

		for chatID, articles := range currentTracking {
			for article, oldPrices := range articles {
				log.Printf("Проверка для chatID %d, артикул %s", chatID, article)
				newProductInfo, err := getWBProductInfo(article)
				if err != nil {
					log.Printf("Ошибка проверки артикула %s: %v", article, err)
					continue
				}

				for _, newSize := range newProductInfo.Sizes {
					newPrice := calculatePrice(newSize.Price)
					oldPrice, ok := oldPrices[newSize.Name]

					if !ok {
						// Новый размер появился в продаже, просто добавляем его
						log.Printf("Появился новый размер %s для артикула %s", newSize.Name, article)
						mu.Lock()
						trackingData[chatID][article][newSize.Name] = newPrice
						mu.Unlock()
						continue
					}

					if newPrice < oldPrice {
						log.Printf("Найдено снижение цены для артикула %s, размер %s!", article, newSize.Name)
						// Цена снизилась! Отправляем уведомление.
						message := fmt.Sprintf(
							"❗️*Снижение цены!*\n\nТовар: *%s*\nАртикул: `%s`\nРазмер: *%s*\n\nСтарая цена: `%.2f BYN`\nНовая цена: `%.2f BYN`",
							newProductInfo.Name,
							article,
							newSize.Name,
							oldPrice,
							newPrice,
						)
						msg := tgbotapi.NewMessage(chatID, message)
						msg.ParseMode = "Markdown"
						bot.Send(msg)

						// Обновляем цену в нашем хранилище, чтобы не спамить
						mu.Lock()
						trackingData[chatID][article][newSize.Name] = newPrice
						mu.Unlock()

						// СОХРАНЯЕМ ОБНОВЛЕННУЮ ЦЕНУ
						if err := saveDataToFile(); err != nil {
							log.Printf("ОШИБКА: не удалось сохранить обновленную цену: %v", err)
					   	}
					}
				}
				// Пауза между запросами, чтобы не забанили
				time.Sleep(2 * time.Second)
			}
		}
	}
}


// Функция для отображения списка отслеживаемых товаров
func handleListRequest(bot *tgbotapi.BotAPI, chatID int64) {
	mu.RLock() // Используем RLock (чтение), так как мы только смотрим данные
	defer mu.RUnlock()

	userTrackingData, ok := trackingData[chatID]
	if !ok || len(userTrackingData) == 0 {
		bot.Send(tgbotapi.NewMessage(chatID, "Вы пока не отслеживаете ни одного товара. Отправьте мне артикул, чтобы начать."))
		return
	}

	var responseText strings.Builder
	responseText.WriteString("Вы отслеживаете следующие товары:\n\n")

	// Пробегаемся по всем артикулам, которые отслеживает пользователь
	for article, sizes := range userTrackingData {
		// Получаем актуальное название товара (можно было бы и сохранить его при первом запросе)
		product, err := getWBProductInfo(article)
		if err != nil {
			responseText.WriteString(fmt.Sprintf("🔴 *Артикул:* `%s` (не удалось получить информацию)\n", article))
			continue
		}

		responseText.WriteString(fmt.Sprintf("✅ *Товар:* %s\n*Артикул:* `%s`\n", product.Name, article))
		
        // Выводим цены по размерам
		for sizeName, price := range sizes {
			responseText.WriteString(fmt.Sprintf(" - Размер *%s*: `%.2f BYN`\n", sizeName, price))
		}
		responseText.WriteString("\n")
	}

	msg := tgbotapi.NewMessage(chatID, responseText.String())
	msg.ParseMode = "Markdown"
	bot.Send(msg)
}


// ПРАВИЛЬНАЯ ВЕРСИЯ handleUntrackRequest
func handleUntrackRequest(bot *tgbotapi.BotAPI, chatID int64, article string) {
    article = strings.TrimSpace(article)
    if article == "" {
        bot.Send(tgbotapi.NewMessage(chatID, "Пожалуйста, укажите артикул, который хотите перестать отслеживать. Например: /untrack 123456"))
        return
    }

    var foundAndDeleted bool
    // --- Начало критической секции ---
    mu.Lock()
    if userTracking, ok := trackingData[chatID]; ok {
        if _, ok := userTracking[article]; ok {
            delete(userTracking, article)
            foundAndDeleted = true
        }
    }
    mu.Unlock()
    // --- Конец критической секции ---

    if foundAndDeleted {
        bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("Больше не отслеживаю товар с артикулом %s.", article)))
        // Сохраняем только если что-то действительно удалили
        if err := saveDataToFile(); err != nil {
            log.Printf("ОШИБКА: не удалось сохранить данные после удаления товара: %v", err)
        }
    } else {
        bot.Send(tgbotapi.NewMessage(chatID, fmt.Sprintf("Вы и не отслеживали товар с артикулом %s.", article)))
    }
}