-- catalog: product reference data. One row per value of common.v1.ProductType
-- (proto/common/v1/product.proto), which fixes the list of products:
--
--   id          the enum number (the stable id every service and event uses)
--   key         the value name without PRODUCT_TYPE_, lower case, "_" -> "-"
--               (the front-end key: PRODUCT_TYPE_ACHCHIQ_QALAMPIR -> achchiq-qalampir)
--   name_uz/ru  the uz / ru labels of the value's comment in product.proto
--   category    the product.proto section the value is listed in
--   sort_order  the enum number x 10, so products can be re-ordered later
--               without renumbering
--
-- A product added to the enum later gets its row in a new migration; the
-- unit test in migrations/seed_test.go fails until every defined value has
-- exactly one row. Rows are never deleted or renumbered: a product that is
-- no longer offered is retired (is_retired = true).

-- +goose Up

INSERT INTO product_types (id, key, name_uz, name_ru, category, sort_order) VALUES
    -- Grains & industrial crops (don va texnika ekinlari)
    (1, 'bugdoy', 'Bug''doy', 'Пшеница', 'GRAIN_INDUSTRIAL', 10), -- wheat
    (2, 'arpa', 'Arpa', 'Ячмень', 'GRAIN_INDUSTRIAL', 20), -- barley
    (3, 'sholi', 'Sholi', 'Рис', 'GRAIN_INDUSTRIAL', 30), -- rice, paddy
    (4, 'makkajoxori', 'Makkajo''xori', 'Кукуруза', 'GRAIN_INDUSTRIAL', 40), -- maize
    (5, 'paxta', 'Paxta', 'Хлопок', 'GRAIN_INDUSTRIAL', 50), -- cotton
    (6, 'kungaboqar', 'Kungaboqar', 'Подсолнечник', 'GRAIN_INDUSTRIAL', 60), -- sunflower
    (7, 'kunjut', 'Kunjut', 'Кунжут', 'GRAIN_INDUSTRIAL', 70), -- sesame
    (8, 'beda', 'Beda', 'Люцерна', 'GRAIN_INDUSTRIAL', 80), -- alfalfa
    -- Vegetables (sabzavot)
    (9, 'kartoshka', 'Kartoshka', 'Картофель', 'VEGETABLE', 90), -- potato
    (10, 'sabzi', 'Sabzi', 'Морковь', 'VEGETABLE', 100), -- carrot
    (11, 'piyoz', 'Piyoz', 'Лук', 'VEGETABLE', 110), -- onion
    (12, 'sarimsoq', 'Sarimsoq', 'Чеснок', 'VEGETABLE', 120), -- garlic
    (13, 'pomidor', 'Pomidor', 'Помидор', 'VEGETABLE', 130), -- tomato
    (14, 'bodring', 'Bodring', 'Огурец', 'VEGETABLE', 140), -- cucumber
    (15, 'karam', 'Karam', 'Капуста', 'VEGETABLE', 150), -- cabbage
    (16, 'qalampir', 'Bulg''or qalampiri', 'Сладкий перец', 'VEGETABLE', 160), -- bell pepper
    (17, 'achchiq-qalampir', 'Achchiq qalampir', 'Острый перец', 'VEGETABLE', 170), -- hot pepper
    (18, 'baqlajon', 'Baqlajon', 'Баклажан', 'VEGETABLE', 180), -- eggplant
    (19, 'qovoq', 'Qovoq', 'Тыква', 'VEGETABLE', 190), -- pumpkin
    (20, 'turp', 'Turp', 'Редька', 'VEGETABLE', 200), -- radish
    (21, 'lavlagi', 'Lavlagi', 'Свёкла', 'VEGETABLE', 210), -- beet
    -- Legumes (dukkakli)
    (22, 'mosh', 'Mosh', 'Маш', 'LEGUME', 220), -- mung bean
    (23, 'noxat', 'No''xat', 'Нут', 'LEGUME', 230), -- chickpea
    (24, 'loviya', 'Loviya', 'Фасоль', 'LEGUME', 240), -- bean
    -- Melons & gourds (poliz)
    (25, 'qovun', 'Qovun', 'Дыня', 'MELON', 250), -- melon
    (26, 'tarvuz', 'Tarvuz', 'Арбуз', 'MELON', 260), -- watermelon
    -- Fruit & berries (meva) — perennial, no seed-based crop norms
    (27, 'uzum', 'Uzum', 'Виноград', 'FRUIT', 270), -- grape
    (28, 'olma', 'Olma', 'Яблоко', 'FRUIT', 280), -- apple
    (29, 'nok', 'Nok', 'Груша', 'FRUIT', 290), -- pear
    (30, 'behi', 'Behi', 'Айва', 'FRUIT', 300), -- quince
    (31, 'shaftoli', 'Shaftoli', 'Персик', 'FRUIT', 310), -- peach
    (32, 'orik', 'O''rik', 'Абрикос', 'FRUIT', 320), -- apricot
    (33, 'gilos', 'Gilos', 'Черешня', 'FRUIT', 330), -- sweet cherry
    (34, 'olcha', 'Olcha', 'Вишня', 'FRUIT', 340), -- sour cherry
    (35, 'olxori', 'Olxo''ri', 'Слива', 'FRUIT', 350), -- plum
    (36, 'anor', 'Anor', 'Гранат', 'FRUIT', 360), -- pomegranate
    (37, 'xurmo', 'Xurmo', 'Хурма', 'FRUIT', 370), -- persimmon
    (38, 'anjir', 'Anjir', 'Инжир', 'FRUIT', 380), -- fig
    (39, 'tut', 'Tut', 'Шелковица', 'FRUIT', 390), -- mulberry
    (40, 'limon', 'Limon', 'Лимон', 'FRUIT', 400), -- lemon
    (41, 'qulupnay', 'Qulupnay', 'Клубника', 'FRUIT', 410), -- strawberry
    -- Nuts (yong'oq) — perennial
    (42, 'yongoq', 'Yong''oq', 'Грецкий орех', 'NUT', 420), -- walnut
    (43, 'bodom', 'Bodom', 'Миндаль', 'NUT', 430), -- almond
    (44, 'pista', 'Pista', 'Фисташка', 'NUT', 440); -- pistachio

-- +goose Down

-- Fails while listings reference these products (listings.product_type).
DELETE FROM product_types WHERE id BETWEEN 1 AND 44;
