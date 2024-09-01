from flask import Flask, request, jsonify
import redis

app = Flask(__name__)

# Setup Redis connection (Adjust host, port, and db according to your configuration)
redis_client = redis.StrictRedis(host='localhost', port=6379, db=0, decode_responses=True)

@app.route('/token', methods=['GET'])
def get_token_data():
    token_public_key = request.args.get('publicKey')
    if not token_public_key:
        return "Public Key parameter is missing.", 400

    # Fetch data from Redis
    token_data = redis_client.get(token_public_key)
    if token_data:
        return jsonify(token_data)
    else:
        return "Token data not found.", 404

if __name__ == '__main__':
    app.run(debug=True)
