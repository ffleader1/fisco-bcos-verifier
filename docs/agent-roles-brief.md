# BouyUp — Vai trò các AI agent

## Tóm tắt nhanh

**Agent 1 – Xác minh**
- Kiểm tra hồ sơ đủ và khớp, thiếu thì yêu cầu bổ sung
- Xin ủy quyền tra cứu tín dụng, dòng tiền, trích nợ
- Xác minh cửa hàng có thật, hoạt động bao lâu, điểm đánh giá
- Lấy điểm tín dụng qua tổ chức có giấy phép (kể cả Ant Group qua 钱塘征信)
- Lấy tín hiệu gian lận từ FATE (buff đơn, đơn không khớp vận đơn)
- Đối chiếu số liệu khai báo với thực tế
- Gian lận / dữ liệu giả thì từ chối luôn

**Agent 2 – Tư vấn & Đề xuất**
- Nói chuyện với người bán như chatbot
- Dựa vào hạn mức tối đa và mức rủi ro từ Machine Learning để quyết có cho vay không, bao nhiêu
- Báo người bán: được vay bao nhiêu, lãi suất, tổng chi phí, lịch trả
- Giải thích vì sao không được vay cao hơn, cần bổ sung gì

**Agent 3 – Kiểm soát**
- Kiểm tra đề xuất của Agent 2 theo luật và chính sách
- Tự đánh giá rủi ro độc lập, tìm lý do để không duyệt
- Tranh luận với Agent 2; hai bên đồng ý thì chốt duyệt ngay

**Agent 4 – Giải ngân & Thu nợ**
- Chuyển tiền 1 lần, thẳng tới nhà cung cấp
- Theo dõi nhà cung cấp giao hàng đúng hạn
- Theo dõi trả nợ, nhắc lịch, giải thích từng lần trích tiền
- Giám sát và cảnh báo Xanh / Vàng / Đỏ
- Soạn phương án giãn nợ để cán bộ duyệt

**Agent 5 – Hậu kiểm**
- Xem lại các khoản đã cho vay, so với kết quả trả nợ thật
- Tìm chỗ hệ thống hay đánh giá sai
- Theo dõi nợ xấu, quá mức thì tạm dừng duyệt tự động
- Đề xuất cải tiến cho các agent khác (người duyệt trước khi áp dụng)

```
Người bán nộp hồ sơ
  → [1] Xác minh ── gian lận / dữ liệu sai → từ chối
  → Machine Learning tính mức rủi ro, hạn mức tối đa, lãi suất
  → [2] Đề xuất khoản vay  ⇄  [3] Kiểm soát   (2 bên cùng đồng ý → duyệt ngay)
  → Người bán bấm chấp nhận
  → [4] Giải ngân, thu nợ, giám sát
  → [5] Hậu kiểm định kỳ
```

---

## Nguyên tắc chung

| Nguyên tắc | Vì sao |
|---|---|
| **Duyệt tự động là mặc định.** Khoản vay được duyệt ngay khi: dữ liệu đã xác minh + Machine Learning cho phép + luật cố định pass + **Agent 2 và Agent 3 cùng đồng ý**. | Giữ tinh thần duyệt nhanh, one click. Hai agent độc lập kiểm tra chéo nhau, đáp ứng yêu cầu tách thẩm định khỏi phê duyệt. |
| **Agent không tự tính con số.** Hạn mức tối đa, lãi suất, điểm rủi ro do Machine Learning và luật cố định tính. | Con số phải nhất quán và kiểm toán được. LLM có thể cho hai kết quả khác nhau với cùng một hồ sơ. |
| **Nói "không" thì dễ, nói "có" thì cần đủ điều kiện.** Một agent được quyền từ chối hoặc hạ mức; muốn duyệt thì phải đủ cả 4 điều kiện ở trên. | Một agent bị lừa hoặc sai cũng không làm ra tiền được. |
| **Từ chối luôn đi kèm lý do và đường đi tiếp:** nộp thêm thông tin qua kênh thủ công. | Luật (Điều 24 Luật BVTTCN, quy định cho vay 2024) bắt buộc có kênh để con người xem xét lại. |
| **Mọi bước đều ghi lên blockchain**, không sửa được. | Kiểm toán, giải trình khi có tranh chấp. |
| **Agent không đọc tài liệu gốc**, chỉ đọc thông tin đã được trích xuất sẵn. | Tránh bị lừa bằng nội dung chèn trong tài liệu. |

---

## Agent 1 — Xác minh

**Làm gì:** mọi việc kiểm tra dữ liệu đầu vào.
- Hồ sơ đủ và khớp nhau; còn thiếu thì yêu cầu bổ sung.
- Xin ủy quyền tra cứu tín dụng, dòng tiền, trích nợ.
- Cửa hàng có thật không, hoạt động bao lâu, điểm đánh giá, vi phạm.
- Điểm tín dụng qua tổ chức có giấy phép (Ngân hàng Nhân dân, 百行征信, 朴道征信, 钱塘征信 của Ant Group).
- Tín hiệu từ FATE: buff đơn, đơn không khớp vận đơn, điểm rủi ro kết hợp.
- Đối chiếu số liệu: doanh số khai báo ↔ vận đơn, tiền nền tảng ↔ tiền về tài khoản; chứng từ nhập hàng, hợp đồng nhà cung cấp có khớp nhau không.

**Kết quả:** một hồ sơ "đã xác minh" kèm danh sách cờ bất thường.

**Không làm:** không bàn hạn mức, không nói chuyện tiền với người bán.

**Vì sao tách riêng:** kiểm tra dữ liệu là việc khác hẳn tư vấn khoản vay. Tách ra thì Agent 2 chỉ làm việc trên dữ liệu đã sạch, và lỗi gian lận bị chặn ngay từ cửa.

**Khi không đạt:** gian lận hoặc dữ liệu giả → **từ chối luôn**, mời nộp qua kênh thủ công. Thiếu hồ sơ → yêu cầu bổ sung.

---

## Agent 2 — Tư vấn & Đề xuất

**Làm gì:** là người nói chuyện với người bán (dạng chatbot).
- Nhận hồ sơ đã xác minh và kết quả Machine Learning: mức rủi ro, nhóm khách hàng, hạn mức tối đa, lãi suất.
- Quyết định đề xuất: có cho vay không, bao nhiêu (không vượt hạn mức tối đa), kỳ hạn.
- Báo cho người bán: được vay bao nhiêu, tối đa bao nhiêu, lãi suất, tổng chi phí, lịch trả.
- Giải thích vì sao không được vay cao hơn: nêu yếu tố như "tỷ lệ hoàn hàng cao" hay "lịch sử bán hàng ngắn", không lộ ngưỡng. Gợi ý cần bổ sung gì để được nhiều hơn.

**Không làm:** không tự kiểm tra lại dữ liệu (việc của Agent 1); không vượt hạn mức tối đa; không tự đặt lãi; không hứa trước khi chốt.

**Vì sao:** người bán cần một đầu mối nói chuyện dễ hiểu. Đề xuất của Agent 2 chưa phải quyết định cuối, phải qua Agent 3.

---

## Agent 3 — Kiểm soát (phản biện)

**Làm gì:** kiểm tra độc lập đề xuất của Agent 2 trước khi chốt.
- **Luật và chính sách:**
  - Lãi suất trong trần.
  - Khoản vay đúng nhóm khách hàng, đúng tầng.
  - Nguồn dữ liệu hợp pháp.
  - Lời giải thích đủ, công bố đủ lãi năm và tổng chi phí.
  - Cam kết (covenants) hợp lý.
- **Rủi ro:** tự đánh giá hồ sơ **trước khi** đọc lập luận của Agent 2, chủ động tìm lý do để không duyệt.
- **Tranh luận:** nếu khác ý, hai agent trao đổi một vòng. Đồng ý thì chốt; vẫn khác ý thì xử lý theo bảng dưới.

**Không làm:** không sửa đề xuất của Agent 2, chỉ đồng ý hoặc phản đối kèm lý do.

**Vì sao:** luật yêu cầu tách thẩm định khỏi phê duyệt. Hai agent độc lập cùng đồng ý thì an toàn mà vẫn nhanh, không cần người ngồi duyệt.

---

## Agent 4 — Giải ngân & Thu nợ

**Làm gì**
- **Giải ngân:** khi người bán chấp nhận, chuyển **1 lần, thẳng tới nhà cung cấp**, đúng số đã chốt. Kiểm tra trước: nhà cung cấp có thật, không liên quan tới người bán, giá hợp lý.
- **Giao hàng:** theo dõi nhà cung cấp có giao đúng hạn không.
- **Thu nợ:** theo dõi trả nợ theo lịch, giải thích từng lần trích tiền, nhắc lịch trả.
- **Giám sát hằng ngày,** cảnh báo 3 mức:
  - Xanh: tự theo dõi.
  - Vàng: viết báo cáo nguyên nhân.
  - Đỏ: báo ngay cán bộ.
- **Giãn nợ:** khi người bán khó khăn thật, soạn phương án giãn nợ để cán bộ duyệt.

**Không làm:** không đổi số tiền hay điều khoản; không tự đòi nợ trước hạn, tự tuyên bố vỡ nợ hay tự giãn nợ; không miễn giảm lãi phí, không đe dọa.

**Vì sao:** đây là các bước động tới tiền và quan hệ lâu dài với khách. Agent làm phần theo dõi, nhắc nhở, đề xuất; quyết định nặng thì con người làm.

---

## Agent 5 — Hậu kiểm

**Làm gì**
- Định kỳ xem lại các khoản đã cho vay: lúc duyệt đánh giá thế nào, thực tế trả nợ ra sao.
- Tìm nhóm hay bị đánh giá sai, cờ hay bị bỏ sót, lời giải thích hay bị khiếu nại.
- Theo dõi nợ xấu của các khoản duyệt tự động; vượt mức thì báo để **tạm dừng duyệt tự động**.
- Thử "tấn công" hệ thống bằng tài liệu giả để kiểm tra độ an toàn.
- Viết đề xuất cải tiến cho các agent khác.

**Không làm:** không tự sửa agent khác khi đang chạy; đề xuất phải được người duyệt và thử nghiệm trước.

**Vì sao:** hệ thống phải tự học từ kết quả thật, nhưng thay đổi phải có kiểm soát.

---

## Khi không đạt thì xử lý thế nào

| Tình huống | Xử lý |
|---|---|
| Gian lận / dữ liệu giả (Agent 1) | Từ chối luôn, mời nộp qua kênh thủ công |
| Số tiền xin vượt ngưỡng tự động | Duyệt ngay phần trong ngưỡng; phần vượt nộp qua kênh thủ công |
| Rủi ro sát mức từ chối | Đề xuất khoản nhỏ hơn thay vì từ chối |
| Agent 2 và Agent 3 vẫn không đồng ý sau tranh luận | Từ chối hoặc đề xuất khoản nhỏ hơn, mời nộp qua kênh thủ công |
| Người bán yêu cầu người xem xét | Chuyển cho cán bộ (bắt buộc theo luật) |
| Nợ xấu nhóm duyệt tự động vượt mức | Tạm dừng duyệt tự động, chuyển hết sang thủ công |

---

## Phần không phải agent

| Thành phần | Vai trò |
|---|---|
| Bộ điều phối (code cố định) | Điều phối các bước, áp bảng xử lý ở trên, ghi mọi thứ lên blockchain |
| Machine Learning | Điểm rủi ro, dự báo doanh số, phát hiện gian lận, lãi suất, hạn mức tối đa |
| Luật cố định | Ngưỡng từ chối, ngưỡng tiền, trần lãi |
| Kênh thủ công + cán bộ WeBank | Nhận hồ sơ bổ sung, xem xét lại, quyết định đòi nợ trước hạn / vỡ nợ / giãn nợ |
| Blockchain (FISCO BCOS) | Sổ cái không sửa được: khoản vay, giải ngân, trả nợ, cảnh báo, dấu vết quyết định |
